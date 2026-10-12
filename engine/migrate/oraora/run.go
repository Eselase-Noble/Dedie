package oraora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// maxStreamRetries is how many times in a row the stream is restarted
// after an error before the job is failed.
const maxStreamRetries = 5

type runner struct {
	jobID  string
	cfg    migrate.Config
	st     *State
	srcCfg connector.Config
	dstCfg connector.Config
	rep    migrate.Reporter
	plan   *Plan
	src    *sql.DB
	dst    *sql.DB
	// applier is the target session that changes are applied on. Its
	// formats are those the source prints keys and values in.
	applier *sql.Conn
	// lookup is a source session for the questions the applying of a
	// change has to ask beyond the row itself; see secondLook.
	lookup *sql.Conn
	// follow reads the source's changes and hands them to this runner
	// to apply. It outlives a restart of the stream.
	follow *orasrc.Follower
}

// Run drives a job from wherever its saved state left it to completion,
// or until the operator pauses or aborts it. It is safe to call again
// after a crash: finished phases are skipped and the stream resumes from
// the checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep}
	return r.run(ctx)
}

func (r *runner) save() { r.rep.SaveState(*r.st) }

func (r *runner) run(ctx context.Context) (migrate.Outcome, error) {
	var err error
	if r.src, err = oracle.Open(ctx, r.srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer r.src.Close()
	if r.dst, err = oracle.Open(ctx, r.dstCfg); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer r.dst.Close()
	defer r.release()
	// What the job found on the target at its start and keeps is left
	// out of the plan for as long as the job lives.
	if r.plan, err = BuildPlan(ctx, r.src, r.dst, origin(r.srcCfg), r.cfg, keptOf(r.st), nil); err != nil {
		return "", err
	}
	if len(r.plan.MapProblems) > 0 {
		return "", fmt.Errorf("preflight failed: Schema mapping: %s", strings.Join(r.plan.MapProblems, "; "))
	}
	if r.rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}

	if !r.st.SchemaDone {
		if err := r.schemaPhase(ctx); err != nil {
			return "", err
		}
	}
	if !r.st.SnapshotDone {
		if err := r.snapshotPhase(ctx); err != nil {
			if errors.Is(err, errInterrupted) {
				if r.rep.Desired() == "abort" {
					return r.abort(ctx), nil
				}
				r.rep.Event("warn", "paused during the initial copy; the copy starts over when the job resumes")
				return migrate.Paused, nil
			}
			return "", err
		}
	}
	if !r.st.IndexingDone {
		r.rep.SetPhase(migrate.PhaseIndexing)
		r.rep.Event("info", "creating stored code, views and synonyms; building indexes and constraints")
		if err := r.applySoft(ctx, r.plan.Code); err != nil {
			return "", err
		}
		if err := r.applySoft(ctx, r.plan.Post); err != nil {
			return "", err
		}
		r.st.IndexingDone = true
		r.save()
	}
	if r.cfg.Offline() {
		// A one-time copy has no changes to follow: validate and finish.
		if _, err := r.finalize(ctx); err != nil {
			return "", err
		}
		return migrate.Completed, nil
	}

	r.follow = r.follower()
	retries := 0
	for {
		r.rep.SetPhase(migrate.PhaseStreaming)
		started := time.Now()
		if err := r.reconnect(ctx); err != nil {
			return "", err
		}
		reason, err := r.follow.Follow(ctx, r.st.MineFrom)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, orasrc.ErrChangesLost) {
				// Trying again cannot bring the changes back.
				return "", err
			}
			if time.Since(started) > time.Minute {
				retries = 0
			}
			retries++
			if retries > maxStreamRetries {
				return "", err
			}
			r.rep.Event("warn", fmt.Sprintf("stream interrupted (%s); reconnecting, attempt %d of %d", err, retries, maxStreamRetries))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		switch reason {
		case orasrc.StopPause:
			r.rep.Event("info", "paused; the job can resume for as long as the source still has its redo logs from "+r.st.AppliedLSN+" onwards")
			return migrate.Paused, nil
		case orasrc.StopAbort:
			return r.abort(ctx), nil
		case orasrc.StopCutover:
			done, err := r.finalize(ctx)
			if err != nil {
				return "", err
			}
			if done {
				return migrate.Completed, nil
			}
		}
	}
}

// reconnect gives the stream fresh sessions: one on the target to apply
// changes on, and none yet on the source for looking rows up again.
func (r *runner) reconnect(ctx context.Context) error {
	r.release()
	conn, err := orasrc.Pinned(ctx, r.dst)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	r.applier = conn
	return nil
}

// release returns the stream's sessions to their pools.
func (r *runner) release() {
	if r.applier != nil {
		r.applier.Close()
		r.applier = nil
	}
	if r.lookup != nil {
		r.lookup.Close()
		r.lookup = nil
	}
}

// applySoft runs DDL on the target. A statement marked soft that fails is
// reported and skipped; any other failure stops the job.
func (r *runner) applySoft(ctx context.Context, list []Stmt) error {
	failed, err := r.apply(ctx, list)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return failed[0].err
	}
	return nil
}

// failure is a statement that must succeed and did not.
type failure struct {
	stmt Stmt
	err  error
}

// apply runs DDL on the target and returns the statements that failed
// and are not soft. Each runs with its object's schema as the current
// one, which is where the names in a view or a trigger that carry no
// schema are looked up; it matters when the schema is not the account's
// own. The error is set when the job cannot go on at all.
func (r *runner) apply(ctx context.Context, list []Stmt) ([]failure, error) {
	if len(list) == 0 {
		return nil, nil
	}
	conn, err := r.dst.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("target: %w", oracle.Friendly(err))
	}
	defer func() {
		// The session goes back to the pool as it came.
		conn.ExecContext(context.WithoutCancel(ctx), "ALTER SESSION SET CURRENT_SCHEMA = "+oracle.Quote(r.plan.Target.User))
		conn.Close()
	}()
	var failed []failure
	current := ""
	for _, s := range list {
		if s.Schema != current {
			if _, err := conn.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+oracle.Quote(s.Schema)); err != nil {
				return nil, fmt.Errorf("target: %w", oracle.Friendly(err))
			}
			current = s.Schema
		}
		_, err := conn.ExecContext(ctx, s.SQL)
		// ORA-24344: created, but it does not compile yet. Everything is
		// compiled again, and what stays invalid reported, at the end.
		if err == nil || alreadyThere(err) || hasCode(err, "ORA-24344") {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !s.Soft {
			failed = append(failed, failure{s, fmt.Errorf("create %s %s: %w", s.Kind, s.Label(), oracle.Friendly(err))})
			continue
		}
		r.rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Label(), oracle.Friendly(err)))
	}
	return failed, nil
}

// ours reports whether the target holds this job's checkpoint, which is
// the mark it leaves there before creating anything.
func (r *runner) ours(ctx context.Context) bool {
	var n int
	_ = r.dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+r.plan.checkpointName()+` WHERE job_id = :1`, r.jobID).Scan(&n)
	return n > 0
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	report, clashes, err := preflight(ctx, r.src, r.dst, r.plan, r.cfg)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own objects on
	// the target. The two checks on what the target already holds are
	// the ones it may ignore.
	retry := r.st.ExistingDecided || r.ours(ctx)
	var blocking []string
	for _, c := range report.Checks {
		if c.Status == "fail" && !(retry && (c.Name == "Target is empty" || c.Name == "Existing objects")) {
			blocking = append(blocking, c.Name+": "+c.Detail)
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
	}
	_, srcVer, _ := oracle.Version(ctx, r.src)
	_, dstVer, _ := oracle.Version(ctx, r.dst)
	r.st.SourceVersion, r.st.TargetVersion = "Oracle "+srcVer, "Oracle "+dstVer

	if !retry {
		if err := r.settleExisting(ctx, clashes); err != nil {
			return err
		}
	}
	r.st.ExistingDecided = true
	r.save()

	// Mark the target as ours before creating anything, so that a retry
	// can tell its own leftovers from what was there before. An SCN of
	// zero means "nothing applied yet".
	if err := r.ensureCheckpoint(ctx, r.dst); err != nil {
		return err
	}
	if _, err := r.dst.ExecContext(ctx, `MERGE INTO `+r.plan.checkpointName()+` t USING (SELECT :1 job_id FROM dual) s ON (t.job_id = s.job_id)
		WHEN NOT MATCHED THEN INSERT (job_id, scn) VALUES (s.job_id, 0)`, r.jobID); err != nil {
		return fmt.Errorf("prepare target: %w", oracle.Friendly(err))
	}
	if !r.cfg.Offline() {
		if err := r.logKeys(ctx); err != nil {
			return err
		}
	}

	r.rep.Event("info", fmt.Sprintf("creating %d table(s) on the target", len(r.plan.Tables)))
	if err := r.createTables(ctx); err != nil {
		return err
	}
	progress := make([]migrate.TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		progress[i] = migrate.TableProgress{Schema: t.Schema, Table: t.Name, EstRows: t.EstRows}
	}
	r.rep.ResetTables(progress)
	r.st.SchemaDone = true
	r.save()
	return nil
}

// settleExisting acts, once, on what the target already held under the
// names of objects to be migrated. Under the default policy there is
// nothing to do: preflight has refused to start. Kept objects are
// written to the job's state and the plan is drawn up again without
// them; objects to be replaced are dropped, to be created like any other.
//
// It must happen before the job creates anything. Afterwards the job's
// own objects stand on the target too, and what was there before can no
// longer be told from them, so every later phase and every resumed run
// goes by what is remembered here.
func (r *runner) settleExisting(ctx context.Context, clashes []clash) error {
	if len(clashes) == 0 {
		return nil
	}
	switch r.cfg.Existing() {
	case migrate.ExistingSkip:
		tables, others := r.st.keep(clashes, r.plan)
		r.st.ExistingDecided = true
		r.save()
		plan, err := BuildPlan(ctx, r.src, r.dst, origin(r.srcCfg), r.cfg, keptOf(r.st), r.plan)
		if err != nil {
			return err
		}
		r.plan = plan
		r.rep.Event("info", fmt.Sprintf("kept %d existing table(s) and %d other object(s) on the target exactly as they are, as chosen; they take no part in the migration", tables, others))
	case migrate.ExistingReplace:
		tables, others, err := dropClashes(ctx, r.dst, clashes)
		if tables+others > 0 {
			r.rep.Event("info", fmt.Sprintf("dropped %d existing table(s) and %d other object(s) on the target, as chosen", tables, others))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// createTables creates the sequences and tables. A table can need stored
// code to exist first: a virtual column or a default that calls a
// function of the schema. Stored code is otherwise created after the
// copy, so when tables fail it is created early, which does no harm, and
// those tables are tried once more.
func (r *runner) createTables(ctx context.Context) error {
	failed, err := r.apply(ctx, r.plan.Pre)
	if err != nil || len(failed) == 0 {
		return err
	}
	if len(r.plan.Code) == 0 {
		return failed[0].err
	}
	r.rep.Event("info", fmt.Sprintf("%d table(s) could not be created yet (%s); creating stored code first and trying again", len(failed), failed[0].err))
	if err := r.applySoft(ctx, r.plan.Code); err != nil {
		return err
	}
	again := make([]Stmt, len(failed))
	for i, f := range failed {
		again[i] = f.stmt
	}
	return r.applySoft(ctx, again)
}

// logKeys switches on primary key logging on the source for the tables
// that need it, and remembers which, so that it can be switched off again.
func (r *runner) logKeys(ctx context.Context) error {
	added := 0
	err := orasrc.LogKeys(ctx, r.src, r.plan.Catalog, func(table string) {
		r.st.LogGroups = append(r.st.LogGroups, table)
		r.save()
		added++
	})
	if err != nil {
		return err
	}
	if added > 0 {
		r.rep.Event("info", fmt.Sprintf("switched on primary key supplemental logging for %d table(s) on the source", added))
	}
	return nil
}

// unlogKeys switches off the logging this job switched on.
func (r *runner) unlogKeys(ctx context.Context) {
	if len(r.st.LogGroups) == 0 || r.cfg.KeepSlot {
		return
	}
	left, err := orasrc.UnlogKeys(ctx, r.src, r.st.LogGroups)
	if err != nil {
		r.rep.Event("warn", "could not connect to the source to switch primary key logging off again: "+err.Error())
		return
	}
	if len(left) > 0 {
		r.rep.Event("warn", "primary key supplemental logging is still on for: "+strings.Join(left, ", ")+
			". Switch it off with ALTER TABLE ... DROP SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS when convenient")
	}
	r.st.LogGroups = left
}

func (r *runner) snapshotPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSnapshot)
	// Start clean: rows left by an interrupted attempt belong to a
	// snapshot that is no longer the one being copied. Only the tables
	// this job created are in the plan.
	for _, t := range r.plan.Tables {
		if _, err := r.dst.ExecContext(ctx, "TRUNCATE TABLE "+t.QName()); err != nil {
			return fmt.Errorf("empty target table %s: %w", t.Target(), oracle.Friendly(err))
		}
	}
	if _, err := r.dst.ExecContext(ctx, `UPDATE `+r.plan.checkpointName()+` SET scn = 0 WHERE job_id = :1`, r.jobID); err != nil {
		return oracle.Friendly(err)
	}

	scn, mineFrom, err := orasrc.StartPosition(ctx, r.src, r.plan.Catalog)
	if err != nil {
		return err
	}
	r.st.SnapshotSCN, r.st.MineFrom = scn, mineFrom
	r.st.ConsistentLSN = fmt.Sprintf("SCN %d", scn)
	r.save()
	r.rep.Event("info", fmt.Sprintf("copying %d table(s) as of SCN %d with %d worker(s)", len(r.plan.Tables), scn, r.cfg.SnapshotWorkers))
	if err := snapshot(ctx, r.src, r.dst, r.plan, scn, r.cfg.SnapshotWorkers, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

// finalize runs once there is nothing left to apply: at cutover, or
// straight after the copy of an offline migration. It returns false when
// validation fails during an online migration, in which case the job
// goes back to streaming.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(migrate.PhaseCutover)
	n, err := r.syncSequences(ctx)
	if err != nil {
		r.rep.Event("warn", "sequence sync incomplete: "+err.Error())
	}
	r.rep.Event("info", fmt.Sprintf("synchronized %d sequence(s) and identity column(s); validating tables (%s)", n, r.cfg.Validation))

	// An offline copy is compared with the source as it was when copied,
	// so writes made since do not count against it.
	var asOf int64
	if r.cfg.Offline() {
		asOf = r.st.SnapshotSCN
	}
	mismatches, err := validate(ctx, r.src, r.dst, r.plan, r.cfg.Validation, asOf, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d table(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if r.cfg.Offline() {
			return false, errors.New(why + ": the source was probably written to during the copy. Stop writes to the source and run the migration again")
		}
		r.rep.SetDesired("run")
		r.rep.Event("error", "cutover cancelled: "+why+". If the source is still receiving writes, stop them and try again; streaming continues")
		r.save()
		return false, nil
	}

	// From here on no more rows are stored, so what would have been in
	// their way can be put in place.
	if len(r.plan.Final) > 0 {
		keys, triggers := 0, map[string]bool{}
		for _, s := range r.plan.Final {
			if s.Object == "trigger" {
				triggers[s.Label()] = true
			} else {
				keys++
			}
		}
		r.rep.Event("info", fmt.Sprintf("adding %d foreign key(s) and %d trigger(s)", keys, len(triggers)))
		if err := r.applySoft(ctx, r.plan.Final); err != nil {
			return false, err
		}
	}
	r.restoreAlways(ctx)
	r.compile(ctx)
	r.gatherStatistics(ctx)
	r.cleanup(ctx)
	r.rep.SetPhase(migrate.PhaseDone)
	if r.cfg.Offline() {
		r.rep.Event("info", "offline copy complete: the target holds the source as of the snapshot")
	} else {
		r.rep.Event("info", "cutover complete: the target is ready for application traffic")
	}
	r.save()
	return true, nil
}

// cleanup removes the checkpoint from the target and the logging this
// job added on the source. The checkpoint table goes with its last row;
// another job may be using it.
func (r *runner) cleanup(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	r.release()
	r.unlogKeys(ctx)
	name := r.plan.checkpointName()
	if _, err := r.dst.ExecContext(ctx, `DELETE FROM `+name+` WHERE job_id = :1`, r.jobID); err != nil {
		if !notThere(err) {
			r.rep.Event("warn", "could not remove the checkpoint from the target: "+oracle.Friendly(err).Error())
		}
		return
	}
	var left int
	if r.dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+name).Scan(&left) == nil && left == 0 {
		r.dst.ExecContext(ctx, `DROP TABLE `+name+` PURGE`)
	}
}

// abort stops the job. Data already copied to the target is left for the
// operator to inspect or drop.
func (r *runner) abort(ctx context.Context) migrate.Outcome {
	r.cleanup(ctx)
	r.rep.Event("info", "aborted. Tables already copied to the target were left in place")
	r.save()
	return migrate.Aborted
}
