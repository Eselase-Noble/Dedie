package mypg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

// maxStreamRetries is how many times in a row the stream is restarted after
// an error before the job is failed.
const maxStreamRetries = 5

type runner struct {
	jobID  string
	cfg    migrate.Config
	st     *mymy.State
	srcCfg connector.Config
	dstCfg connector.Config
	rep    migrate.Reporter
	plan   *Plan
	src    *sql.DB
	dst    *pgx.Conn
}

// Run drives a job from wherever its saved state left it to completion, or
// until the operator pauses or aborts it. It is safe to call again after a
// crash: finished phases are skipped and the stream resumes from the
// checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st mymy.State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep}
	out, err := r.run(ctx)
	// Never leave the source read-only behind a job that did not finish.
	if err != nil && st.Fenced {
		if uerr := mymy.Unfence(ctx, srcCfg); uerr != nil {
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
	if r.src, err = mysql.Open(ctx, r.srcCfg, nil); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer r.src.Close()
	if r.dst, err = postgres.Connect(ctx, r.dstCfg); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer func() { r.dst.Close(context.WithoutCancel(ctx)) }()
	if r.plan, err = BuildPlan(ctx, r.src, r.srcCfg.Database, r.cfg); err != nil {
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
		if err := r.reconnect(ctx); err != nil {
			return "", err
		}
		failed, err := applyStmts(ctx, r.dst, r.plan.Post)
		if err != nil {
			return "", err
		}
		for _, s := range failed {
			r.rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Name, pgMessage(s.err)))
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
		case mymy.StopPause:
			r.rep.Event("info", "paused; the job can resume for as long as the source still has its binary logs from "+r.st.Applied.File+" onwards")
			return migrate.Paused, nil
		case mymy.StopAbort:
			return r.abort(ctx), nil
		case mymy.StopCutover:
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

// reconnect replaces the target control connection if a long phase left it stale.
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

// applyStmts runs DDL on the target and returns the soft statements that failed.
func applyStmts(ctx context.Context, dst *pgx.Conn, list []Stmt) ([]Stmt, error) {
	var failed []Stmt
	for _, s := range list {
		_, err := dst.PgConn().Exec(ctx, s.SQL).ReadAll()
		if err == nil || alreadyExists(err) {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !s.Soft {
			return nil, fmt.Errorf("create %s %s: %s", s.Kind, s.Name, pgMessage(err))
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
	return mymy.Stream(ctx, r.jobID, r.cfg, r.st, r.plan.Plan, r.srcCfg, target, r.rep)
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	report, err := preflight(ctx, r.src, r.dst, r.plan, r.cfg)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own tables on the
	// target; that is the one failed check it may ignore.
	var ours bool
	_ = r.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+metaSchema+`.mysql_checkpoint WHERE job_id = $1)`, r.jobID).Scan(&ours)
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
	_ = r.src.QueryRowContext(ctx, "SELECT VERSION()").Scan(&srcVer)
	_ = r.dst.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&dstVer)
	r.st.SourceVersion, r.st.TargetVersion = "MySQL "+srcVer, "PostgreSQL "+dstVer

	// Mark the target as ours before creating anything, so that a retry can
	// tell its own leftovers from somebody else's tables.
	if _, err := r.dst.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return fmt.Errorf("prepare target: %s", pgMessage(err))
	}
	if _, err := r.dst.Exec(ctx, `INSERT INTO `+metaSchema+`.mysql_checkpoint (job_id, binlog_file, binlog_pos) VALUES ($1, '', 0) ON CONFLICT DO NOTHING`, r.jobID); err != nil {
		return err
	}

	r.rep.Event("info", fmt.Sprintf("creating %d translated tables on the target", len(r.plan.Tables)))
	if _, err := applyStmts(ctx, r.dst, r.plan.Pre); err != nil {
		return err
	}
	progress := make([]migrate.TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		progress[i] = migrate.TableProgress{Schema: targetSchema, Table: t.Name, EstRows: t.EstRows}
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
	names := make([]string, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		names[i] = qname(t.Name)
	}
	if _, err := r.dst.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" CASCADE"); err != nil {
		return fmt.Errorf("empty target tables: %s", pgMessage(err))
	}
	if _, err := r.dst.Exec(ctx, `UPDATE `+metaSchema+`.mysql_checkpoint SET binlog_file = '', binlog_pos = 0 WHERE job_id = $1`, r.jobID); err != nil {
		return err
	}
	conns, pos, err := mymy.OpenSnapshot(ctx, r.src, r.cfg.SnapshotWorkers)
	if err != nil {
		return err
	}
	defer func() {
		// End the snapshot transactions before the connections go back to
		// the pool; left open they would block the read-only switch at cutover.
		for _, c := range conns {
			c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			c.Close()
		}
	}()
	r.st.Start, r.st.ConsistentLSN = pos, pos.String()
	r.save()
	r.rep.Event("info", fmt.Sprintf("copying %d tables from a snapshot at %s with %d workers", len(r.plan.Tables), pos, len(conns)))
	if err := snapshot(ctx, conns, r.dstCfg, r.plan, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

// syncIdentities moves each identity column past the source's AUTO_INCREMENT
// counter, so the target never hands out an id the source already used.
func (r *runner) syncIdentities(ctx context.Context) (int, error) {
	conn, err := r.src.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
	rows, err := mymy.QueryStrings(ctx, conn, `SELECT table_name, auto_increment FROM information_schema.tables
		WHERE table_schema = ? AND auto_increment IS NOT NULL`, r.plan.Database)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		t := r.plan.Table(row[0])
		if t == nil {
			continue
		}
		var next uint64
		if _, err := fmt.Sscan(row[1], &next); err != nil {
			continue
		}
		for _, c := range t.Cols {
			if !c.AutoIncrement {
				continue
			}
			if _, err := r.dst.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s RESTART WITH %d", qname(t.Name), qident(c.Name), next)); err != nil {
				return n, fmt.Errorf("%s: %s", t.Name, pgMessage(err))
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
	n, err := r.syncIdentities(ctx)
	if err != nil {
		r.rep.Event("warn", "identity sync incomplete: "+err.Error())
	}
	r.rep.Event("info", fmt.Sprintf("synchronized %d identity columns; validating tables (%s)", n, r.cfg.Validation))

	mismatches, err := validate(ctx, r.src, r.dstCfg, r.plan, r.cfg.Validation, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d table(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if r.st.Fenced {
			if uerr := mymy.Unfence(ctx, r.srcCfg); uerr != nil {
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

	// Planner statistics do not exist yet for the new tables.
	if _, err := r.dst.Exec(ctx, "ANALYZE"); err != nil {
		r.rep.Event("warn", "ANALYZE on the target failed: "+pgMessage(err))
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

// cleanup removes the checkpoint from the target.
func (r *runner) cleanup(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	dst, err := postgres.Connect(ctx, r.dstCfg)
	if err != nil {
		r.rep.Event("warn", "could not connect to the target to remove the checkpoint: "+pgMessage(err))
		return
	}
	defer dst.Close(ctx)
	var exists bool
	_ = dst.QueryRow(ctx, `SELECT to_regclass('`+metaSchema+`.mysql_checkpoint') IS NOT NULL`).Scan(&exists)
	if !exists {
		return
	}
	dst.Exec(ctx, `DELETE FROM `+metaSchema+`.mysql_checkpoint WHERE job_id = $1`, r.jobID)
	var left int
	if dst.QueryRow(ctx, `SELECT count(*) FROM `+metaSchema+`.mysql_checkpoint`).Scan(&left) == nil && left == 0 {
		dst.Exec(ctx, `DROP TABLE `+metaSchema+`.mysql_checkpoint`)
		// The schema is shared with other pipelines; drop it only when empty.
		dst.Exec(ctx, `DROP SCHEMA `+metaSchema)
	}
}

// abort stops the job. Data already copied to the target is left for the
// operator to inspect or drop.
func (r *runner) abort(ctx context.Context) migrate.Outcome {
	if r.st.Fenced {
		if err := mymy.Unfence(ctx, r.srcCfg); err != nil {
			r.rep.Event("error", "could not make the source writable again: "+err.Error())
		} else {
			r.st.Fenced = false
		}
	}
	r.cleanup(ctx)
	r.rep.Event("info", "aborted. Tables already copied to the target were left in place")
	r.save()
	return migrate.Aborted
}
