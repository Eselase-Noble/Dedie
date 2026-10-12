package orapg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/pgpg"
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
	dst    *pgx.Conn
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

// buildPlan reads the source catalog and takes out of the plan what the
// job leaves as the target has it: a kept table is excluded like any table
// the operator excluded, and a kept object loses its statement.
func (r *runner) buildPlan(ctx context.Context) error {
	cfg := r.cfg
	cfg.ExcludeTables = append(cfg.ExcludeTables[:len(cfg.ExcludeTables):len(cfg.ExcludeTables)], r.st.SkippedTables...)
	plan, err := BuildPlan(ctx, r.src, cfg)
	if err != nil {
		// With every table kept there is nothing to plan; say why,
		// rather than that the schema appears to be empty.
		if len(r.st.SkippedTables) > 0 && strings.Contains(err.Error(), "there are no tables to migrate") {
			return pgpg.ErrNothingLeft
		}
		return err
	}
	plan.keep(r.st.KeptObjects)
	r.plan = plan
	return nil
}

func (r *runner) run(ctx context.Context) (migrate.Outcome, error) {
	var err error
	if r.src, err = oracle.Open(ctx, r.srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer r.src.Close()
	if r.rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}
	if r.dst, err = postgres.Connect(ctx, r.dstCfg); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer func() { r.dst.Close(context.WithoutCancel(ctx)) }()
	if err := r.buildPlan(ctx); err != nil {
		return "", err
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
		r.rep.Event("info", "building indexes and check constraints")
		if err := r.reconnect(ctx); err != nil {
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

// reconnect replaces the target connection if a long phase left it stale.
func (r *runner) reconnect(ctx context.Context) error {
	if r.dst.Ping(ctx) == nil {
		return nil
	}
	r.dst.Close(ctx)
	c, err := postgres.Connect(ctx, r.dstCfg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	r.dst = c
	return nil
}

// applySoft runs DDL on the target. A statement marked soft that fails is
// reported and skipped; any other failure stops the job.
func (r *runner) applySoft(ctx context.Context, list []Stmt) error {
	for _, s := range list {
		_, err := r.dst.PgConn().Exec(ctx, s.SQL).ReadAll()
		if err == nil || alreadyExists(err) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !s.Soft {
			return fmt.Errorf("create %s %s: %s", s.Kind, s.Name, pgMessage(err))
		}
		r.rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Name, pgMessage(err)))
	}
	return nil
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	report, err := preflight(ctx, r.src, r.dst, r.plan, r.cfg)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own sequences
	// and tables on the target. The two checks on what the target already
	// holds are the ones it may ignore.
	var ours bool
	_ = r.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+metaSchema+`.oracle_checkpoint WHERE job_id = $1)`, r.jobID).Scan(&ours)
	var blocking []string
	for _, c := range report.Checks {
		if c.Status == "fail" && !(ours && (c.Name == "Target is empty" || c.Name == "Existing objects")) {
			blocking = append(blocking, c.Name+": "+c.Detail)
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
	}
	// What was on the target before the job is settled now, while it can
	// still be told from what the job creates, and remembered from here on.
	if !ours {
		found, err := pgpg.FindExisting(ctx, r.dst, r.plan.claims())
		if err != nil {
			return err
		}
		var skipped []string
		skipped, r.st.KeptObjects, r.st.DropFirst = pgpg.Decide(r.cfg.Existing(), found)
		r.st.SkippedTables = r.plan.sources(skipped)
		r.save()
		if n, m := len(r.st.SkippedTables), len(r.st.KeptObjects); n+m > 0 {
			if err := r.buildPlan(ctx); err != nil {
				return err
			}
			r.rep.Event("warn", fmt.Sprintf("kept %d existing table(s) and %d other object(s) on the target as they are, as chosen; they are left out of the migration", n, m))
			if len(r.plan.Tables) == 0 {
				return pgpg.ErrNothingLeft
			}
		}
	}
	_, srcVer, _ := oracle.Version(ctx, r.src)
	var dstVer string
	_ = r.dst.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&dstVer)
	r.st.SourceVersion, r.st.TargetVersion = "Oracle "+srcVer, "PostgreSQL "+dstVer

	// Mark the target as ours before creating anything, so that a retry
	// can tell its own leftovers from somebody else's tables. An SCN of
	// zero means "nothing applied yet".
	if _, err := r.dst.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return fmt.Errorf("prepare target: %s", pgMessage(err))
	}
	if _, err := r.dst.Exec(ctx, `INSERT INTO `+metaSchema+`.oracle_checkpoint (job_id, scn) VALUES ($1, 0) ON CONFLICT DO NOTHING`, r.jobID); err != nil {
		return err
	}
	if err := pgpg.DropExisting(ctx, r.dst, r.st.DropFirst, r.rep); err != nil {
		return err
	}
	if !r.cfg.Offline() {
		if err := r.logKeys(ctx); err != nil {
			return err
		}
	}

	r.rep.Event("info", fmt.Sprintf("creating %d translated table(s) on the target", len(r.plan.Tables)))
	if err := r.applySoft(ctx, r.plan.Pre); err != nil {
		return err
	}
	progress := make([]migrate.TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		progress[i] = migrate.TableProgress{Schema: t.Schema, Table: t.Target, EstRows: t.EstRows}
	}
	r.rep.ResetTables(progress)
	r.st.SchemaDone = true
	r.save()
	return nil
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
	// snapshot that is no longer the one being copied.
	names := make([]string, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		names[i] = t.QName()
	}
	if _, err := r.dst.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" CASCADE"); err != nil {
		return fmt.Errorf("empty target tables: %s", pgMessage(err))
	}
	if _, err := r.dst.Exec(ctx, `UPDATE `+metaSchema+`.oracle_checkpoint SET scn = 0 WHERE job_id = $1`, r.jobID); err != nil {
		return err
	}

	scn, mineFrom, err := orasrc.StartPosition(ctx, r.src, r.plan.Catalog)
	if err != nil {
		return err
	}
	r.st.SnapshotSCN, r.st.MineFrom = scn, mineFrom
	r.st.ConsistentLSN = fmt.Sprintf("SCN %d", scn)
	r.save()
	r.rep.Event("info", fmt.Sprintf("copying %d table(s) as of SCN %d with %d worker(s)", len(r.plan.Tables), scn, r.cfg.SnapshotWorkers))
	if err := snapshot(ctx, r.src, r.dstCfg, r.plan, scn, r.cfg.SnapshotWorkers, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

// syncSequences moves every sequence and identity column on the target
// to where the source's counter stands, so the target never hands out a
// number the source already used.
func (r *runner) syncSequences(ctx context.Context) (int, error) {
	next, err := orasrc.SequencePositions(ctx, r.src, r.plan.Catalog)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range r.plan.Sequences {
		v, ok := next[s.Owner+"."+s.Name]
		if !ok {
			continue
		}
		if _, err := r.dst.Exec(ctx, "ALTER SEQUENCE "+pgx.Identifier{s.Schema, s.Target}.Sanitize()+" RESTART WITH "+v); err != nil {
			return n, fmt.Errorf("%s.%s: %s", s.Schema, s.Target, pgMessage(err))
		}
		n++
	}
	for _, t := range r.plan.Tables {
		for _, c := range t.Cols {
			v, ok := next[t.Owner+"."+c.IdentitySeq]
			if c.IdentitySeq == "" || !ok {
				continue
			}
			if _, err := r.dst.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s RESTART WITH %s", t.QName(), qident(c.Target), v)); err != nil {
				return n, fmt.Errorf("%s: %s", t.Label(), pgMessage(err))
			}
			n++
		}
	}
	return n, nil
}

// finalize runs once there is nothing left to apply: at cutover, or
// straight after the copy of an offline migration. It returns false when
// validation fails during an online migration, in which case the job
// goes back to streaming.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(migrate.PhaseCutover)
	if err := r.reconnect(ctx); err != nil {
		return false, err
	}
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
	mismatches, err := validate(ctx, r.src, r.dstCfg, r.plan, r.cfg.Validation, asOf, r.rep)
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

	if len(r.plan.Final) > 0 {
		r.rep.Event("info", fmt.Sprintf("adding %d foreign key(s)", len(r.plan.Final)))
		if err := r.applySoft(ctx, r.plan.Final); err != nil {
			return false, err
		}
	}
	if err := r.finishViews(ctx); err != nil {
		return false, err
	}
	// Planner statistics do not exist yet for the new tables.
	if _, err := r.dst.Exec(ctx, "ANALYZE"); err != nil {
		r.rep.Event("warn", "ANALYZE on the target failed: "+pgMessage(err))
	}
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
// job added on the source.
func (r *runner) cleanup(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	r.unlogKeys(ctx)
	dst, err := postgres.Connect(ctx, r.dstCfg)
	if err != nil {
		r.rep.Event("warn", "could not connect to the target to remove the checkpoint: "+pgMessage(err))
		return
	}
	defer dst.Close(ctx)
	var exists bool
	_ = dst.QueryRow(ctx, `SELECT to_regclass('`+metaSchema+`.oracle_checkpoint') IS NOT NULL`).Scan(&exists)
	if !exists {
		return
	}
	dst.Exec(ctx, `DELETE FROM `+metaSchema+`.oracle_checkpoint WHERE job_id = $1`, r.jobID)
	var left int
	if dst.QueryRow(ctx, `SELECT count(*) FROM `+metaSchema+`.oracle_checkpoint`).Scan(&left) == nil && left == 0 {
		dst.Exec(ctx, `DROP TABLE `+metaSchema+`.oracle_checkpoint`)
		// The schema is shared with other pipelines; drop it only when empty.
		dst.Exec(ctx, `DROP SCHEMA `+metaSchema)
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
