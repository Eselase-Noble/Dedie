package pgpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// maxStreamRetries is how many times in a row the stream is restarted after
// an error (a dropped connection, a failover) before the job is failed.
const maxStreamRetries = 5

type runner struct {
	jobID  string
	cfg    Config
	st     *State
	srcCfg connector.Config
	dstCfg connector.Config
	rep    Reporter
	plan   *Plan
	src    *pgx.Conn
	dst    *pgx.Conn
}

// Run drives a job from wherever its saved state left it to completion, or
// until the operator pauses or aborts it. It is safe to call again after a
// crash: finished phases are skipped and the stream resumes from the
// checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg Config, st State, srcCfg, dstCfg connector.Config, rep Reporter) (Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep}
	out, err := r.run(ctx)
	// Never leave the source read-only behind a job that did not finish.
	if err != nil && st.Fenced {
		if uerr := unfence(ctx, srcCfg); uerr != nil {
			rep.Event("error", "the source database is still read-only and could not be reset: "+pgMessage(uerr))
		} else {
			st.Fenced = false
			rep.Event("warn", "the job stopped during cutover; the source database is writable again")
		}
		rep.SaveState(st)
	}
	return out, err
}

func (r *runner) run(ctx context.Context) (Outcome, error) {
	jobID, cfg, st, srcCfg, dstCfg, rep := r.jobID, r.cfg, r.st, r.srcCfg, r.dstCfg, r.rep
	if st.Slot == "" {
		name := "dedie_" + strings.Map(func(c rune) rune {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
				return c
			}
			return '_'
		}, strings.ToLower(jobID))
		st.Slot, st.Publication = name, name
	}
	if rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}

	var err error
	if r.src, err = postgres.Connect(ctx, srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer func() { r.src.Close(context.WithoutCancel(ctx)) }()
	if r.dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer func() { r.dst.Close(context.WithoutCancel(ctx)) }()
	if r.plan, err = BuildPlan(ctx, r.src, cfg); err != nil {
		return "", err
	}

	if !st.SchemaDone {
		if err := r.schemaPhase(ctx); err != nil {
			return "", err
		}
	}
	if !st.SnapshotDone {
		if err := r.snapshotPhase(ctx); err != nil {
			if errors.Is(err, errInterrupted) {
				if rep.Desired() == "abort" {
					return r.abort(ctx), nil
				}
				rep.Event("warn", "paused during the initial copy; the copy starts over when the job resumes")
				return Paused, nil
			}
			return "", err
		}
	}
	if !st.IndexingDone {
		if err := r.indexingPhase(ctx); err != nil {
			return "", err
		}
	}

	retries := 0
	for {
		rep.SetPhase(PhaseStreaming)
		started := time.Now()
		reason, err := stream(ctx, jobID, cfg, r.st, srcCfg, dstCfg, rep)
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
			rep.Event("warn", fmt.Sprintf("stream interrupted (%s); reconnecting, attempt %d of %d", err, retries, maxStreamRetries))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		switch reason {
		case stopPause:
			rep.Event("info", "paused; the source keeps WAL for this job until it resumes or is aborted")
			return Paused, nil
		case stopAbort:
			return r.abort(ctx), nil
		case stopCutover:
			done, err := r.finalize(ctx)
			if err != nil {
				return "", err
			}
			if done {
				return Completed, nil
			}
		}
	}
}

func (r *runner) save() { r.rep.SaveState(*r.st) }

// reconnect replaces control connections that went stale during a long phase.
func (r *runner) reconnect(ctx context.Context) error {
	if r.src.Ping(ctx) != nil {
		r.src.Close(ctx)
		c, err := postgres.Connect(ctx, r.srcCfg)
		if err != nil {
			return fmt.Errorf("source: %w", err)
		}
		r.src = c
	}
	if r.dst.Ping(ctx) != nil {
		r.dst.Close(ctx)
		c, err := postgres.Connect(ctx, r.dstCfg)
		if err != nil {
			return fmt.Errorf("target: %w", err)
		}
		r.dst = c
	}
	return nil
}

func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(PhaseSchema)
	report, err := preflight(ctx, r.src, r.dst, r.plan)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own tables on the
	// target; that is the one failed check it may ignore.
	ours := r.ownTarget(ctx)
	var blocking []string
	for _, c := range report.Checks {
		if c.Status == "fail" && !(ours && c.Name == "Target is empty") {
			blocking = append(blocking, c.Name+": "+c.Detail)
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
	}
	_ = r.src.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&r.st.SourceVersion)
	_ = r.dst.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&r.st.TargetVersion)

	// Mark the target as ours before creating anything, so that a retry can
	// tell its own leftovers from somebody else's tables.
	if _, err := r.dst.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return fmt.Errorf("prepare target: %s", pgMessage(err))
	}
	if _, err := r.dst.Exec(ctx, `INSERT INTO `+metaSchema+`.checkpoint (job_id, commit_lsn, end_lsn) VALUES ($1, '0/0', '0/0') ON CONFLICT DO NOTHING`, r.jobID); err != nil {
		return err
	}

	r.rep.Event("info", fmt.Sprintf("creating schema on the target: %d tables", len(r.plan.Tables)))
	if _, err := applyStmts(ctx, r.dst, r.plan.Pre); err != nil {
		return err
	}

	names := make([]string, len(r.plan.Tables))
	progress := make([]TableProgress, len(r.plan.Tables))
	for i, t := range r.plan.Tables {
		names[i] = "ONLY " + t.QName()
		progress[i] = TableProgress{Schema: t.Schema, Table: t.Name, EstRows: t.EstRows}
	}
	if _, err := r.src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+qident(r.st.Publication)); err != nil {
		return err
	}
	if _, err := r.src.Exec(ctx, "CREATE PUBLICATION "+qident(r.st.Publication)+" FOR TABLE "+strings.Join(names, ", ")); err != nil {
		return fmt.Errorf("create publication: %s", pgMessage(err))
	}
	r.rep.ResetTables(progress)
	r.st.SchemaDone = true
	r.save()
	return nil
}

// ownTarget reports whether this job already started preparing the target.
func (r *runner) ownTarget(ctx context.Context) bool {
	var mine bool
	err := r.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+metaSchema+`.checkpoint WHERE job_id=$1)`, r.jobID).Scan(&mine)
	return err == nil && mine
}

func (r *runner) snapshotPhase(ctx context.Context) error {
	r.rep.SetPhase(PhaseSnapshot)
	// Start clean: a slot or rows left by an interrupted attempt belong to a
	// snapshot that no longer exists.
	if err := dropSlot(ctx, r.src, r.st.Slot); err != nil {
		return err
	}
	if err := truncateTargets(ctx, r.dst, r.plan); err != nil {
		return fmt.Errorf("empty target tables: %s", pgMessage(err))
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
		return fmt.Errorf("create replication slot: %s", pgMessage(err))
	}
	r.st.ConsistentLSN = slot.ConsistentPoint
	r.save()
	r.rep.Event("info", fmt.Sprintf("copying %d tables from a snapshot at %s with %d workers", len(r.plan.Tables), slot.ConsistentPoint, r.cfg.SnapshotWorkers))

	if err := snapshot(ctx, r.srcCfg, r.dstCfg, slot.SnapshotName, r.plan, r.cfg.SnapshotWorkers, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "initial copy finished")
	return nil
}

func (r *runner) indexingPhase(ctx context.Context) error {
	r.rep.SetPhase(PhaseIndexing)
	if err := r.reconnect(ctx); err != nil {
		return err
	}
	// Soft statements from the first phase get a second chance now that the
	// tables exist; anything already created is skipped.
	var list []Stmt
	for _, s := range r.plan.Pre {
		if s.Soft {
			list = append(list, s)
		}
	}
	// With foreign keys in place, a replicated parent change would cascade
	// on the target and then collide with the replicated child change. The
	// replica role switches those actions off for the apply session; where
	// the target user may not set it, foreign keys wait until cutover.
	if _, err := r.dst.Exec(ctx, "SET session_replication_role = replica"); err == nil {
		r.st.ReplicaRole = true
		r.dst.Exec(ctx, "RESET session_replication_role")
	} else {
		r.rep.Event("warn", "target user may not set session_replication_role; foreign keys will be created at cutover instead of now")
	}
	for _, s := range r.plan.Post {
		if s.Kind != kindForeignKey || r.st.ReplicaRole {
			list = append(list, s)
		}
	}
	r.rep.Event("info", "building indexes, foreign keys and views")
	failed, err := applyStmts(ctx, r.dst, list)
	if err != nil {
		return err
	}
	reportFailed(r.rep, failed)
	r.st.IndexingDone = true
	r.save()
	return nil
}

// finalize runs once the stream has drained at cutover. It returns false
// when validation fails, in which case the job goes back to streaming.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(PhaseCutover)
	if err := r.reconnect(ctx); err != nil {
		return false, err
	}
	n, err := syncSequences(ctx, r.src, r.dst, r.plan.Schemas)
	if err != nil {
		r.rep.Event("warn", "sequence sync incomplete: "+err.Error())
	}
	r.rep.Event("info", fmt.Sprintf("synchronized %d sequences; validating tables (%s)", n, r.cfg.Validation))

	var srcNum, dstNum int
	_ = r.src.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&srcNum)
	_ = r.dst.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&dstNum)
	floatsDiffer := srcNum < 120000 || dstNum < 120000
	mismatches, err := validate(ctx, r.srcCfg, r.dstCfg, r.plan, r.cfg.Validation, floatsDiffer, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d table(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if r.st.Fenced {
			if uerr := unfence(ctx, r.srcCfg); uerr != nil {
				r.rep.Event("error", "could not make the source writable again: "+pgMessage(uerr))
			} else {
				r.st.Fenced = false
			}
		}
		r.rep.SetDesired("run")
		r.rep.Event("error", "cutover cancelled: "+why+". If the source is still receiving writes, stop them and try again; streaming continues")
		r.save()
		return false, nil
	}

	final := r.plan.Final
	if !r.st.ReplicaRole {
		for _, s := range r.plan.Post {
			if s.Kind == kindForeignKey {
				final = append(final, s)
			}
		}
	}
	failed, err := applyStmts(ctx, r.dst, final)
	if err != nil {
		return false, err
	}
	reportFailed(r.rep, failed)
	// Planner statistics are not replicated; without them the first queries
	// after cutover can pick very poor plans.
	if _, err := r.dst.Exec(ctx, "ANALYZE"); err != nil {
		r.rep.Event("warn", "ANALYZE on the target failed: "+pgMessage(err))
	}
	if r.cfg.KeepSlot {
		r.rep.Event("warn", "replication slot "+r.st.Slot+" was kept as requested; the source retains WAL until it is dropped")
	} else {
		cleanup(ctx, r.jobID, r.st, r.srcCfg, r.dstCfg, r.rep)
	}
	r.rep.SetPhase(PhaseDone)
	msg := "cutover complete: the target is ready for application traffic"
	if r.st.Fenced {
		msg += ". The source database was left read-only"
	}
	r.rep.Event("info", msg)
	r.save()
	return true, nil
}

// abort stops the job and removes its footprint on the source. Data already
// copied to the target is left for the operator to inspect or drop.
func (r *runner) abort(ctx context.Context) Outcome {
	if r.st.Fenced {
		if err := unfence(ctx, r.srcCfg); err != nil {
			r.rep.Event("error", "could not make the source writable again: "+pgMessage(err))
		} else {
			r.st.Fenced = false
		}
	}
	cleanup(ctx, r.jobID, r.st, r.srcCfg, r.dstCfg, r.rep)
	r.rep.Event("info", "aborted; replication slot and publication removed from the source. Tables already copied to the target were left in place")
	r.save()
	return Aborted
}
