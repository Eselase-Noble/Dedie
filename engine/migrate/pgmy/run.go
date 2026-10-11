package pgmy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// maxStreamRetries is how many times in a row the stream is restarted after
// an error before the job is failed.
const maxStreamRetries = 5

type runner struct {
	jobID  string
	cfg    migrate.Config
	st     *pgpg.State
	srcCfg connector.Config
	dstCfg connector.Config
	rep    migrate.Reporter
	plan   *Plan
	src    *pgx.Conn
	dst    *sql.DB
}

// Run drives a job from wherever its saved state left it to completion, or
// until the operator pauses or aborts it. It is safe to call again after a
// crash: finished phases are skipped and the stream resumes from the
// checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st pgpg.State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	if st.Slot == "" {
		st.Slot, st.Publication = pgpg.SlotName(jobID), pgpg.SlotName(jobID)
	}
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep}
	out, err := r.run(ctx)
	// Never leave the source read-only behind a job that did not finish.
	if err != nil && st.Fenced {
		if uerr := pgpg.Unfence(ctx, srcCfg); uerr != nil {
			rep.Event("error", "the source database is still read-only and could not be reset: "+uerr.Error())
		} else {
			st.Fenced = false
			rep.Event("warn", "the job stopped during cutover; the source database is writable again")
		}
		rep.SaveState(st)
	}
	return out, err
}

func (r *runner) save() { r.rep.SaveState(*r.st) }

func (r *runner) run(ctx context.Context) (migrate.Outcome, error) {
	if r.rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}
	var err error
	if r.src, err = postgres.Connect(ctx, r.srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer func() { r.src.Close(context.WithoutCancel(ctx)) }()
	if r.dst, err = mysql.Open(ctx, r.dstCfg, targetParams); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer r.dst.Close()
	if r.plan, err = BuildPlan(ctx, r.src, r.cfg); err != nil {
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
		r.rep.Event("info", "building indexes and foreign keys")
		failed, err := applyStmts(ctx, r.dst, r.plan.Post)
		if err != nil {
			return "", err
		}
		for _, s := range failed {
			r.rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Name, message(s.err)))
		}
		r.st.IndexingDone = true
		r.save()
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
		case pgpg.StopPause:
			r.rep.Event("info", "paused; the source keeps WAL for this job until it resumes or is aborted")
			return migrate.Paused, nil
		case pgpg.StopAbort:
			return r.abort(ctx), nil
		case pgpg.StopCutover:
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

// applyStmts runs DDL on the target and returns the soft statements that failed.
func applyStmts(ctx context.Context, dst *sql.DB, list []Stmt) ([]Stmt, error) {
	var failed []Stmt
	for _, s := range list {
		_, err := dst.ExecContext(ctx, s.SQL)
		if err == nil || alreadyExists(err) {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !s.Soft {
			return nil, fmt.Errorf("create %s %s: %s", s.Kind, s.Name, message(err))
		}
		s.err = err
		failed = append(failed, s)
	}
	return failed, nil
}

func (r *runner) stream(ctx context.Context) (string, error) {
	target, err := newApplier(ctx, r.dstCfg, r.jobID, r.plan, func(msg string) {
		r.st.Anomalies++
		if r.st.Anomalies <= 20 {
			r.rep.Event("warn", msg)
		}
	})
	if err != nil {
		return "", err
	}
	defer target.Close()
	return pgpg.Stream(ctx, r.jobID, r.cfg, r.st, r.srcCfg, target, r.rep)
}

// reconnect replaces the source control connection if a long phase left it stale.
func (r *runner) reconnect(ctx context.Context) error {
	if r.src.Ping(ctx) == nil {
		return nil
	}
	r.src.Close(ctx)
	c, err := postgres.Connect(ctx, r.srcCfg)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	r.src = c
	return nil
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	report, err := preflight(ctx, r.src, r.dst, r.plan)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own tables on the
	// target; that is the one failed check it may ignore.
	var ours bool
	_ = r.dst.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM `"+checkpointTable+"` WHERE job_id = ?", r.jobID).Scan(&ours)
	var blocking []string
	for _, c := range report.Checks {
		if c.Status == "fail" && !(ours && c.Name == "Target is empty") {
			blocking = append(blocking, c.Name+": "+c.Detail)
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
	}
	var srcVer, dstVer string
	_ = r.src.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&srcVer)
	_ = r.dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&dstVer)
	r.st.SourceVersion, r.st.TargetVersion = "PostgreSQL "+srcVer, "MySQL "+dstVer

	// Mark the target as ours before creating anything, so that a retry can
	// tell its own leftovers from somebody else's tables.
	if _, err := r.dst.ExecContext(ctx, checkpointDDL); err != nil {
		return fmt.Errorf("prepare target: %s", message(err))
	}
	if _, err := r.dst.ExecContext(ctx, "INSERT IGNORE INTO `"+checkpointTable+"` (job_id, commit_lsn, end_lsn) VALUES (?, '', '')", r.jobID); err != nil {
		return err
	}

	r.rep.Event("info", fmt.Sprintf("creating %d translated tables on the target", len(r.plan.Tables)))
	if _, err := applyStmts(ctx, r.dst, r.plan.Pre); err != nil {
		return err
	}

	names := make([]string, len(r.plan.Tables))
	progress := make([]migrate.TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		names[i] = "ONLY " + pgx.Identifier{t.Schema, t.Name}.Sanitize()
		progress[i] = migrate.TableProgress{Schema: t.Schema, Table: t.Name, EstRows: t.EstRows}
	}
	publication := pgx.Identifier{r.st.Publication}.Sanitize()
	if _, err := r.src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+publication); err != nil {
		return err
	}
	if _, err := r.src.Exec(ctx, "CREATE PUBLICATION "+publication+" FOR TABLE "+strings.Join(names, ", ")); err != nil {
		return fmt.Errorf("create publication: %w", err)
	}
	r.rep.ResetTables(progress)
	r.st.SchemaDone = true
	r.save()
	return nil
}

func (r *runner) snapshotPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSnapshot)
	// Start clean: a slot or rows left by an interrupted attempt belong to a
	// snapshot that no longer exists.
	if err := pgpg.DropSlot(ctx, r.src, r.st.Slot); err != nil {
		return err
	}
	for _, t := range r.plan.Tables {
		if _, err := r.dst.ExecContext(ctx, "TRUNCATE TABLE "+t.QName()); err != nil {
			return fmt.Errorf("empty target table %s: %s", t.Target, message(err))
		}
	}
	if _, err := r.dst.ExecContext(ctx, "UPDATE `"+checkpointTable+"` SET commit_lsn = '', end_lsn = '' WHERE job_id = ?", r.jobID); err != nil {
		return err
	}

	repl, err := postgres.ConnectReplication(ctx, r.srcCfg)
	if err != nil {
		return fmt.Errorf("source replication connection: %w", err)
	}
	// The exported snapshot stays valid only while this connection sits idle.
	defer repl.Close(context.WithoutCancel(ctx))
	slot, err := pglogrepl.CreateReplicationSlot(ctx, repl, r.st.Slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{
		Mode: pglogrepl.LogicalReplication, SnapshotAction: "EXPORT_SNAPSHOT",
	})
	if err != nil {
		return fmt.Errorf("create replication slot: %w", err)
	}
	r.st.ConsistentLSN = slot.ConsistentPoint
	r.save()
	r.rep.Event("info", fmt.Sprintf("copying %d tables from a snapshot at %s with %d workers", len(r.plan.Tables), slot.ConsistentPoint, r.cfg.SnapshotWorkers))
	if err := snapshot(ctx, r.srcCfg, r.dst, slot.SnapshotName, r.plan, r.cfg.SnapshotWorkers, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

// syncCounters moves each AUTO_INCREMENT counter past the last value its
// source sequence handed out.
func (r *runner) syncCounters(ctx context.Context) (int, error) {
	n := 0
	for _, t := range r.plan.Tables {
		for _, c := range t.Columns {
			if !c.Auto {
				continue
			}
			var last int64
			err := r.src.QueryRow(ctx, `SELECT coalesce((SELECT s.last_value FROM pg_sequences s
				WHERE format('%I.%I', s.schemaname, s.sequencename) = pg_get_serial_sequence($1, $2)), 0)`,
				pgx.Identifier{t.Schema, t.Name}.Sanitize(), c.Name).Scan(&last)
			if err != nil {
				return n, fmt.Errorf("%s.%s: %w", t.Schema, t.Name, err)
			}
			if _, err := r.dst.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", t.QName(), last+1)); err != nil {
				return n, fmt.Errorf("%s: %s", t.Target, message(err))
			}
			n++
		}
	}
	return n, nil
}

// finalize runs once the stream has drained at cutover. It returns false
// when validation fails, in which case the job goes back to streaming.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(migrate.PhaseCutover)
	if err := r.reconnect(ctx); err != nil {
		return false, err
	}
	n, err := r.syncCounters(ctx)
	if err != nil {
		r.rep.Event("warn", "AUTO_INCREMENT sync incomplete: "+err.Error())
	}
	r.rep.Event("info", fmt.Sprintf("synchronized %d AUTO_INCREMENT counters; validating tables (%s)", n, r.cfg.Validation))

	mismatches, err := validate(ctx, r.srcCfg, r.dst, r.plan, r.cfg.Validation, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d table(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if r.st.Fenced {
			if uerr := pgpg.Unfence(ctx, r.srcCfg); uerr != nil {
				r.rep.Event("error", "could not make the source writable again: "+uerr.Error())
			} else {
				r.st.Fenced = false
			}
		}
		r.rep.SetDesired("run")
		r.rep.Event("error", "cutover cancelled: "+why+". If the source is still receiving writes, stop them and try again; streaming continues")
		r.save()
		return false, nil
	}

	for _, t := range r.plan.Tables {
		if rows, err := r.dst.QueryContext(ctx, "ANALYZE TABLE "+t.QName()); err == nil {
			rows.Close()
		}
	}
	r.cleanup(ctx)
	r.rep.SetPhase(migrate.PhaseDone)
	msg := "cutover complete: the target is ready for application traffic"
	if r.st.Fenced {
		msg += ". The source database was left read-only"
	}
	r.rep.Event("info", msg)
	r.save()
	return true, nil
}

// cleanup removes the slot and publication from the source and the
// checkpoint table from the target.
func (r *runner) cleanup(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	pgpg.CleanupSource(ctx, r.st, r.srcCfg, r.rep)
	dst, err := mysql.Open(ctx, r.dstCfg, nil)
	if err != nil {
		r.rep.Event("warn", "could not connect to the target to remove the table "+checkpointTable+": "+message(err))
		return
	}
	defer dst.Close()
	if _, err := dst.ExecContext(ctx, "DROP TABLE IF EXISTS `"+checkpointTable+"`"); err != nil {
		r.rep.Event("warn", "table "+checkpointTable+" was not removed from the target: "+message(err))
	}
}

// abort stops the job and removes its footprint. Data already copied to the
// target is left for the operator to inspect or drop.
func (r *runner) abort(ctx context.Context) migrate.Outcome {
	if r.st.Fenced {
		if err := pgpg.Unfence(ctx, r.srcCfg); err != nil {
			r.rep.Event("error", "could not make the source writable again: "+err.Error())
		} else {
			r.st.Fenced = false
		}
	}
	r.cleanup(ctx)
	r.rep.Event("info", "aborted; replication slot and publication removed from the source. Tables already copied to the target were left in place")
	r.save()
	return migrate.Aborted
}
