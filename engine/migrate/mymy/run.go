package mymy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// maxStreamRetries is how many times in a row the stream is restarted after
// an error (a dropped connection, a failover) before the job is failed.
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
}

// Run drives a job from wherever its saved state left it to completion, or
// until the operator pauses or aborts it. It is safe to call again after a
// crash: finished phases are skipped and the stream resumes from the
// checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep}
	out, err := r.run(ctx)
	// Never leave the source read-only behind a job that did not finish.
	if err != nil && st.Fenced {
		if uerr := Unfence(ctx, srcCfg); uerr != nil {
			rep.Event("error", "the source database is still read-only and could not be reset: "+message(uerr))
		} else {
			st.Fenced = false
			rep.Event("warn", "the job stopped during cutover; the source database is writable again")
		}
		rep.SaveState(st)
	}
	return out, err
}

func (r *runner) save() { r.rep.SaveState(*r.st) }

// buildPlan reads the source catalog and takes out of the plan what the job
// leaves as the target has it: a kept table is excluded like any table the
// operator excluded, and a kept object loses its statement.
func (r *runner) buildPlan(ctx context.Context) error {
	cfg := r.cfg
	cfg.ExcludeTables = append(cfg.ExcludeTables[:len(cfg.ExcludeTables):len(cfg.ExcludeTables)], r.st.SkippedTables...)
	plan, err := BuildPlan(ctx, r.src, r.srcCfg.Database, cfg)
	if err != nil {
		return err
	}
	plan.keep(r.st.KeptObjects)
	r.plan = plan
	return nil
}

func (r *runner) run(ctx context.Context) (migrate.Outcome, error) {
	if r.rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}
	var err error
	if r.src, err = mysql.Open(ctx, r.srcCfg, nil); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer r.src.Close()
	// Tables reference each other in any order, so they are created with
	// foreign-key checks off on these control connections.
	if r.dst, err = mysql.Open(ctx, r.dstCfg, targetParams(map[string]string{"foreign_key_checks": "0"})); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer r.dst.Close()
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
		r.rep.Event("info", "creating views and stored routines")
		failed, err := applyStmts(ctx, r.dst, r.plan.Post)
		if err != nil {
			return "", err
		}
		reportFailed(r.rep, failed)
		r.st.IndexingDone = true
		r.save()
	}

	// An offline job has no stream to follow: the copy is the whole
	// migration, and it finishes without waiting for the operator.
	if r.cfg.Offline() {
		if _, err := r.finalize(ctx); err != nil {
			return "", err
		}
		return migrate.Completed, nil
	}

	retries := 0
	for {
		r.rep.SetPhase(migrate.PhaseStreaming)
		started := time.Now()
		reason, err := r.stream(ctx)
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
		case StopPause:
			r.rep.Event("info", "paused; the job can resume for as long as the source still has its binary logs from "+r.st.Applied.File+" onwards")
			return migrate.Paused, nil
		case StopAbort:
			return r.abort(ctx), nil
		case StopCutover:
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

// stream opens the apply connection to the target and replays the binary log
// into it.
func (r *runner) stream(ctx context.Context) (string, error) {
	target, err := newApplier(ctx, r.dstCfg, r.jobID, func(msg string) {
		r.st.Anomalies++
		if r.st.Anomalies <= 20 {
			r.rep.Event("warn", msg)
		}
	})
	if err != nil {
		return "", err
	}
	defer target.Close()
	return Stream(ctx, r.jobID, r.cfg, r.st, r.plan, r.srcCfg, target, r.rep)
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	report, err := preflight(ctx, r.src, r.dst, r.plan, r.cfg)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own leftovers on
	// the target. The two checks on what the target already holds are the
	// ones it may ignore.
	var ours bool
	_ = r.dst.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM `"+checkpointTable+"` WHERE job_id = ?", r.jobID).Scan(&ours)
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
		found, err := FindExisting(ctx, r.dst, r.plan.objects())
		if err != nil {
			return err
		}
		r.st.SkippedTables, r.st.KeptObjects, r.st.DropFirst = Decide(r.cfg.Existing(), found)
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
	_ = r.src.QueryRowContext(ctx, "SELECT VERSION()").Scan(&r.st.SourceVersion)
	_ = r.dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&r.st.TargetVersion)

	// Mark the target as ours before creating anything, so that a retry can
	// tell its own leftovers from somebody else's tables.
	if _, err := r.dst.ExecContext(ctx, checkpointDDL); err != nil {
		return fmt.Errorf("prepare target: %s", message(err))
	}
	if _, err := r.dst.ExecContext(ctx, "INSERT IGNORE INTO `"+checkpointTable+"` (job_id, binlog_file, binlog_pos) VALUES (?, '', 0)", r.jobID); err != nil {
		return err
	}

	if err := DropExisting(ctx, r.dst, r.st.DropFirst, r.rep); err != nil {
		return err
	}
	r.rep.Event("info", fmt.Sprintf("creating %d tables on the target", len(r.plan.Tables)))
	if _, err := applyStmts(ctx, r.dst, r.plan.Pre); err != nil {
		return err
	}
	progress := make([]migrate.TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		progress[i] = migrate.TableProgress{Schema: r.plan.Database, Table: t.Name, EstRows: t.EstRows}
	}
	r.rep.ResetTables(progress)
	r.st.SchemaDone = true
	r.save()
	return nil
}

func (r *runner) snapshotPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSnapshot)
	// Start clean: rows left by an interrupted attempt belong to a snapshot
	// that no longer exists.
	if err := truncateTargets(ctx, r.dstCfg, r.plan); err != nil {
		return fmt.Errorf("empty target tables: %w", err)
	}
	if _, err := r.dst.ExecContext(ctx, "UPDATE `"+checkpointTable+"` SET binlog_file = '', binlog_pos = 0 WHERE job_id = ?", r.jobID); err != nil {
		return err
	}
	var conns []*sql.Conn
	var pos Position
	var err error
	if r.cfg.Offline() {
		conns, err = OpenOfflineSnapshot(ctx, r.src, r.cfg.SnapshotWorkers)
	} else {
		conns, pos, err = OpenSnapshot(ctx, r.src, r.cfg.SnapshotWorkers)
	}
	if err != nil {
		return err
	}
	defer func() {
		// End the snapshot transactions before the connections go back to
		// the pool. Left open, they would hold metadata locks on every
		// table and block the read-only switch at cutover.
		for _, c := range conns {
			c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			c.Close()
		}
	}()
	if r.cfg.Offline() {
		r.rep.Event("info", fmt.Sprintf("copying %d tables from one snapshot of the source with %d workers", len(r.plan.Tables), len(conns)))
	} else {
		r.st.Start, r.st.ConsistentLSN = pos, pos.String()
		r.save()
		r.rep.Event("info", fmt.Sprintf("copying %d tables from a snapshot at %s with %d workers", len(r.plan.Tables), pos, len(conns)))
	}
	if err := snapshot(ctx, conns, r.dstCfg, r.plan, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

// finalize runs once the stream has drained at cutover, or straight after
// indexing in an offline job. It returns false when validation fails, in
// which case the job goes back to streaming; an offline job has no stream to
// go back to and fails instead.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(migrate.PhaseCutover)
	n, err := syncAutoIncrement(ctx, r.src, r.dst, r.plan)
	if err != nil {
		r.rep.Event("warn", "AUTO_INCREMENT sync incomplete: "+err.Error())
	}
	r.rep.Event("info", fmt.Sprintf("synchronized %d AUTO_INCREMENT counters; validating tables (%s)", n, r.cfg.Validation))

	mismatches, err := validate(ctx, r.src, r.dst, r.plan, r.cfg.Validation, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d table(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if r.cfg.Offline() {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("%s: the source was probably written to during the copy. Stop writes to the source and run the migration again", why)
		}
		if r.st.Fenced {
			if uerr := Unfence(ctx, r.srcCfg); uerr != nil {
				r.rep.Event("error", "could not make the source writable again: "+message(uerr))
			} else {
				r.st.Fenced = false
			}
		}
		r.rep.SetDesired("run")
		r.rep.Event("error", "cutover cancelled: "+why+". If the source is still receiving writes, stop them and try again; streaming continues")
		r.save()
		return false, nil
	}

	failed, err := applyStmts(ctx, r.dst, r.plan.Final)
	if err != nil {
		return false, err
	}
	reportFailed(r.rep, failed)
	// Index statistics are not copied; refresh them so the first queries
	// after cutover are planned sensibly.
	for _, t := range r.plan.Tables {
		if rows, err := r.dst.QueryContext(ctx, "ANALYZE TABLE "+t.QName()); err == nil {
			rows.Close()
		}
	}
	cleanup(ctx, r.dstCfg, r.rep)
	r.rep.SetPhase(migrate.PhaseDone)
	msg := "cutover complete: the target is ready for application traffic"
	if r.st.Fenced {
		msg += ". The source database was left read-only"
	}
	if r.cfg.Offline() {
		msg = "offline copy complete: the target holds the source as of the snapshot"
	}
	r.rep.Event("info", msg)
	r.save()
	return true, nil
}

// abort stops the job. Data already copied to the target is left for the
// operator to inspect or drop.
func (r *runner) abort(ctx context.Context) migrate.Outcome {
	if r.st.Fenced {
		if err := Unfence(ctx, r.srcCfg); err != nil {
			r.rep.Event("error", "could not make the source writable again: "+message(err))
		} else {
			r.st.Fenced = false
		}
	}
	cleanup(ctx, r.dstCfg, r.rep)
	r.rep.Event("info", "aborted. Tables already copied to the target were left in place")
	r.save()
	return migrate.Aborted
}
