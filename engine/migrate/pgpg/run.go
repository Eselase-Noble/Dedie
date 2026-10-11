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
		if uerr := Unfence(ctx, srcCfg); uerr != nil {
			rep.Event("error", "the source database is still read-only and could not be reset: "+pgMessage(uerr))
		} else {
			st.Fenced = false
			rep.Event("warn", "the job stopped during cutover; the source database is writable again")
		}
		rep.SaveState(st)
	}
	return out, err
}

// SlotName derives the name of a job's replication slot and publication.
// Slot names allow only lower-case letters, digits and underscores.
func SlotName(jobID string) string {
	return "dedie_" + strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			return c
		}
		return '_'
	}, strings.ToLower(jobID))
}

func (r *runner) run(ctx context.Context) (Outcome, error) {
	jobID, cfg, st, srcCfg, dstCfg, rep := r.jobID, r.cfg, r.st, r.srcCfg, r.dstCfg, r.rep
	// An offline job follows no changes and so never names a slot.
	if st.Slot == "" && !cfg.Offline() {
		st.Slot, st.Publication = SlotName(jobID), SlotName(jobID)
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
	if err := r.buildPlan(ctx); err != nil {
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

	// An offline job has no stream to follow: the copy is the whole
	// migration, and it finishes without waiting for the operator.
	if cfg.Offline() {
		if _, err := r.finalize(ctx); err != nil {
			return "", err
		}
		return Completed, nil
	}

	retries := 0
	for {
		rep.SetPhase(PhaseStreaming)
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
			rep.Event("warn", fmt.Sprintf("stream interrupted (%s); reconnecting, attempt %d of %d", err, retries, maxStreamRetries))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		switch reason {
		case StopPause:
			rep.Event("info", "paused; the source keeps WAL for this job until it resumes or is aborted")
			return Paused, nil
		case StopAbort:
			return r.abort(ctx), nil
		case StopCutover:
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

// buildPlan reads the source catalog and takes out of the plan what the job
// leaves as the target has it: a kept table is excluded like any table the
// operator excluded, and a kept object loses its statements.
func (r *runner) buildPlan(ctx context.Context) error {
	cfg := r.cfg
	cfg.ExcludeTables = append(cfg.ExcludeTables[:len(cfg.ExcludeTables):len(cfg.ExcludeTables)], r.st.SkippedTables...)
	plan, err := BuildPlan(ctx, r.src, cfg)
	if err != nil {
		return err
	}
	plan.keep(r.st.KeptObjects)
	r.plan = plan
	return nil
}

// stream opens the apply connection to the target and replays the
// replication stream into it.
func (r *runner) stream(ctx context.Context) (string, error) {
	target, err := newApplier(ctx, r.dstCfg, r.jobID, r.st.ReplicaRole, func(msg string) {
		r.st.Anomalies++
		if r.st.Anomalies <= 20 {
			r.rep.Event("warn", msg)
		}
	})
	if err != nil {
		return "", err
	}
	defer target.Close()
	return Stream(ctx, r.jobID, r.cfg, r.st, r.srcCfg, target, r.rep)
}

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
	report, err := preflight(ctx, r.src, r.dst, r.plan, r.cfg)
	if err != nil {
		return err
	}
	// A retry after an interrupted schema phase finds its own tables on the
	// target, and the types, sequences and functions it created before them.
	// The two checks on what the target already holds are the ones it may
	// ignore.
	ours := r.ownTarget(ctx)
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
		found, err := FindExisting(ctx, r.dst, r.plan.claims())
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
				return ErrNothingLeft
			}
		}
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

	if err := DropExisting(ctx, r.dst, r.st.DropFirst, r.rep); err != nil {
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
	// The publication exists for the stream; an offline job creates nothing
	// on the source.
	if !r.cfg.Offline() {
		if _, err := r.src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+qident(r.st.Publication)); err != nil {
			return err
		}
		if _, err := r.src.Exec(ctx, "CREATE PUBLICATION "+qident(r.st.Publication)+" FOR TABLE "+strings.Join(names, ", ")); err != nil {
			return fmt.Errorf("create publication: %s", pgMessage(err))
		}
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
	if r.cfg.Offline() {
		return r.offlineSnapshot(ctx)
	}
	// Start clean: a slot or rows left by an interrupted attempt belong to a
	// snapshot that no longer exists.
	if err := DropSlot(ctx, r.src, r.st.Slot); err != nil {
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

// offlineSnapshot copies the tables for a job that follows no changes. The
// workers share the snapshot of an ordinary read-only transaction instead of
// a replication slot's, so nothing is created on the source.
func (r *runner) offlineSnapshot(ctx context.Context) error {
	if err := truncateTargets(ctx, r.dst, r.plan); err != nil {
		return fmt.Errorf("empty target tables: %s", pgMessage(err))
	}
	holder, name, err := ExportSnapshot(ctx, r.srcCfg)
	if err != nil {
		return err
	}
	// The exported snapshot stays valid only while this transaction is open.
	defer holder.Close(context.WithoutCancel(ctx))
	r.rep.Event("info", fmt.Sprintf("copying %d tables from one snapshot of the source with %d workers", len(r.plan.Tables), r.cfg.SnapshotWorkers))

	if err := snapshot(ctx, r.srcCfg, r.dstCfg, name, r.plan, r.cfg.SnapshotWorkers, r.rep); err != nil {
		return err
	}
	r.st.SnapshotDone = true
	r.save()
	r.rep.Event("info", "copy finished")
	return nil
}

// replicaRole reports whether the target lets a session run with
// session_replication_role = replica. It tries the setting on the session
// and puts it back; nothing in the database changes.
func replicaRole(ctx context.Context, dst *pgx.Conn) bool {
	if _, err := dst.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		return false
	}
	dst.Exec(ctx, "RESET session_replication_role")
	return true
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
	// the target user may not set it, foreign keys wait until cutover. An
	// offline job replays nothing, so its foreign keys never need to wait.
	offline := r.cfg.Offline()
	if !offline {
		if replicaRole(ctx, r.dst) {
			r.st.ReplicaRole = true
		} else {
			r.rep.Event("warn", "target user may not set session_replication_role; foreign keys will be created at cutover instead of now")
		}
	}
	indexing, _ := r.plan.deferred(r.st.ReplicaRole || offline)
	list = append(list, indexing...)
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

// finalize runs once the stream has drained at cutover, or straight after
// indexing in an offline job. It returns false when validation fails, in
// which case the job goes back to streaming; an offline job has no stream to
// go back to and fails instead.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(PhaseCutover)
	if err := r.reconnect(ctx); err != nil {
		return false, err
	}
	n, err := syncSequences(ctx, r.src, r.dst, r.plan)
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
		if r.cfg.Offline() {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("%s: the source was probably written to during the copy. Stop writes to the source and run the migration again", why)
		}
		if r.st.Fenced {
			if uerr := Unfence(ctx, r.srcCfg); uerr != nil {
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

	_, final := r.plan.deferred(r.st.ReplicaRole || r.cfg.Offline())
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
	if r.cfg.KeepSlot && !r.cfg.Offline() {
		r.rep.Event("warn", "replication slot "+r.st.Slot+" was kept as requested; the source retains WAL until it is dropped")
	} else {
		cleanup(ctx, r.jobID, r.st, r.srcCfg, r.dstCfg, r.rep)
	}
	r.rep.SetPhase(PhaseDone)
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

// abort stops the job and removes its footprint on the source. Data already
// copied to the target is left for the operator to inspect or drop.
func (r *runner) abort(ctx context.Context) Outcome {
	if r.st.Fenced {
		if err := Unfence(ctx, r.srcCfg); err != nil {
			r.rep.Event("error", "could not make the source writable again: "+pgMessage(err))
		} else {
			r.st.Fenced = false
		}
	}
	cleanup(ctx, r.jobID, r.st, r.srcCfg, r.dstCfg, r.rep)
	if r.cfg.Offline() {
		r.rep.Event("info", "aborted. Tables already copied to the target were left in place")
	} else {
		r.rep.Event("info", "aborted; replication slot and publication removed from the source. Tables already copied to the target were left in place")
	}
	r.save()
	return Aborted
}
