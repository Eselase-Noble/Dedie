package mongopg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// maxStreamRetries is how many times in a row the stream is restarted
// after an error before the job is failed.
const maxStreamRetries = 5

type runner struct {
	jobID  string
	cfg    migrate.Config
	st     *State
	dstCfg connector.Config
	rep    migrate.Reporter
	scope  scope
	// columns holds the fields lifted into columns, by collection.
	columns map[string][]column
	list    []Collection
	// skipped holds the collections left out because the target's tables
	// of their names are kept; see State.SkippedTables.
	skipped map[string]bool
	client  *mongo.Client
	db      *mongo.Database
	dst     *pgx.Conn
}

// Run drives a job from wherever its saved state left it to completion,
// or until the operator pauses or aborts it. It is safe to call again
// after a crash: finished phases are skipped and the stream resumes from
// the checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, dstCfg: dstCfg, rep: rep, scope: newScope(cfg)}
	if rep.Desired() == "abort" {
		return r.abort(ctx), nil
	}
	var err error
	if r.columns, err = allColumns(cfg); err != nil {
		return "", err
	}
	if r.client, err = mongodb.Connect(ctx, srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer r.client.Disconnect(context.WithoutCancel(ctx))
	r.db = r.client.Database(srcCfg.Database)
	if r.dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer func() { r.dst.Close(context.WithoutCancel(ctx)) }()
	if r.list, err = r.collections(ctx); err != nil {
		return "", err
	}
	if err := checkNames(r.list); err != nil {
		return "", err
	}
	return r.run(ctx)
}

// errNothingLeft stops a job, and its preview, when the target is to keep
// the table of every collection in scope.
var errNothingLeft = errors.New("every collection to be migrated already has a table on the target, which is kept; nothing is left to migrate")

func (r *runner) save() { r.rep.SaveState(*r.st) }

// collections lists the collections the job covers: those in scope, less
// the ones whose tables were on the target before the job and are kept.
func (r *runner) collections(ctx context.Context) ([]Collection, error) {
	list, err := collections(ctx, r.db, r.scope)
	if err != nil {
		return nil, err
	}
	r.skipped = map[string]bool{}
	for _, name := range r.st.SkippedTables {
		r.skipped[name] = true
	}
	out := list[:0]
	for _, c := range list {
		if !r.skipped[c.Name] {
			out = append(out, c)
		}
	}
	return out, nil
}

// copyList is what the copy covers: the collections whose tables the
// schema phase prepared. One created since then has no table yet, and a
// table of its name on the target would not be this job's to empty; its
// documents arrive through the stream, which starts before it existed. An
// offline job has no stream and answers only for what it copies.
func (r *runner) copyList() []Collection {
	if r.st.Tables == nil {
		return r.list
	}
	prepared := map[string]bool{}
	for _, name := range r.st.Tables {
		prepared[name] = true
	}
	var out []Collection
	for _, c := range r.list {
		if prepared[c.Name] {
			out = append(out, c)
		} else if r.cfg.Offline() {
			r.rep.Event("warn", "collection "+c.Name+" was created after the job started and is not part of this copy")
		}
	}
	if r.cfg.Offline() {
		r.list = out
	}
	return out
}

func (r *runner) run(ctx context.Context) (migrate.Outcome, error) {
	if !r.st.SchemaDone {
		if err := r.schemaPhase(ctx); err != nil {
			return "", err
		}
	}
	if !r.st.SnapshotDone {
		r.rep.SetPhase(migrate.PhaseSnapshot)
		list := r.copyList()
		r.rep.Event("info", fmt.Sprintf("copying %d collection(s) with %d worker(s)", len(list), r.cfg.SnapshotWorkers))
		altered, dropped, err := snapshot(ctx, r.db, r.dstCfg, list, r.columns, r.cfg.SnapshotWorkers, r.rep)
		if errors.Is(err, errInterrupted) {
			if r.rep.Desired() == "abort" {
				return r.abort(ctx), nil
			}
			r.rep.Event("warn", "paused during the initial copy; the copy starts over when the job resumes")
			return migrate.Paused, nil
		}
		if err != nil {
			return "", err
		}
		if altered > 0 {
			r.st.Anomalies += altered
			r.rep.Event("warn", fmt.Sprintf("%d document(s) contained null characters, which jsonb cannot store; they were removed from those documents", altered))
		}
		if dropped > 0 {
			r.st.Unconverted += dropped
			r.rep.Event("warn", fmt.Sprintf("%d field value(s) were of a type their column cannot hold and were stored as NULL there; the documents keep the originals", dropped))
		}
		r.st.SnapshotDone = true
		r.save()
		if r.cfg.Offline() {
			r.rep.Event("info", "copy finished")
		} else {
			r.rep.Event("info", "initial copy finished; following changes")
		}
	}

	// An offline job has no stream to follow: the copy is the whole
	// migration, and it finishes without waiting for the operator.
	if r.cfg.Offline() {
		if err := r.reconnect(ctx); err != nil {
			return "", err
		}
		if _, err := r.finalize(ctx); err != nil {
			return "", err
		}
		return migrate.Completed, nil
	}

	retries := 0
	for {
		r.rep.SetPhase(migrate.PhaseStreaming)
		started := time.Now()
		if err := r.reconnect(ctx); err != nil {
			return "", err
		}
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
		case stopPause:
			r.save()
			r.rep.Event("info", "paused; the job can resume for as long as the source keeps its oplog from this point")
			return migrate.Paused, nil
		case stopAbort:
			return r.abort(ctx), nil
		case stopCutover:
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

// schemaPhase records where the stream must start and creates the tables.
func (r *runner) schemaPhase(ctx context.Context) error {
	r.rep.SetPhase(migrate.PhaseSchema)
	r.st.SourceVersion, _ = mongodb.Version(ctx, r.client)
	_ = r.dst.QueryRow(ctx, "SHOW server_version").Scan(&r.st.TargetVersion)
	r.st.SourceVersion, r.st.TargetVersion = "MongoDB "+r.st.SourceVersion, "PostgreSQL "+r.st.TargetVersion

	// The cluster time is taken before anything is copied. Every change
	// from here on is replayed, including those the copy also sees. An
	// offline job replays nothing, and a standalone server reports no time.
	if !r.cfg.Offline() {
		raw, err := r.db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Raw()
		if err != nil {
			return mongodb.Friendly(err)
		}
		t, i, ok := raw.Lookup("operationTime").TimestampOK()
		if !ok {
			return errors.New("the source did not report a cluster time; it must be a replica set member")
		}
		r.st.StartT, r.st.StartI = t, i
	}

	// The tables must be new, unless the job was told to keep or replace
	// what is there, or they are this job's own: a retry after an
	// interrupted schema phase finds the tables it created the first time.
	// What was there before the job is settled now, while it can still be
	// told from what the job creates, and remembered from here on.
	var ours bool
	_ = r.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+metaSchema+`.mongo_checkpoint WHERE job_id=$1)`, r.jobID).Scan(&ours)
	if !ours {
		found, err := pgpg.FindExisting(ctx, r.dst, claims(r.list))
		if err != nil {
			return err
		}
		report := &migrate.Report{}
		pgpg.ReportExisting(report, found, r.cfg.Existing(), targetPlace)
		var blocking []string
		for _, c := range report.Checks {
			if c.Status == "fail" {
				blocking = append(blocking, c.Name+": "+c.Detail)
			}
		}
		if len(blocking) > 0 {
			return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
		}
		r.st.SkippedTables, _, r.st.DropFirst = pgpg.Decide(r.cfg.Existing(), found)
		r.save()
		if n := len(r.st.SkippedTables); n > 0 {
			if r.list, err = r.collections(ctx); err != nil {
				return err
			}
			r.rep.Event("warn", fmt.Sprintf("kept %d existing table(s) on the target as they are, as chosen; their collections are left out of the migration", n))
			if len(r.list) == 0 {
				return errNothingLeft
			}
		}
	}
	// Mark the target as ours before creating anything.
	if _, err := r.dst.Exec(ctx, checkpointDDL); err != nil {
		return err
	}
	if _, err := r.dst.Exec(ctx, checkpointMark, r.jobID); err != nil {
		return err
	}
	if err := pgpg.DropExisting(ctx, r.dst, r.st.DropFirst, r.rep); err != nil {
		return err
	}

	progress := make([]migrate.TableProgress, len(r.list))
	r.st.Tables = make([]string, len(r.list))
	for n, c := range r.list {
		if _, err := r.dst.Exec(ctx, tableDDL(c.Name, r.columns[c.Name])); err != nil {
			return fmt.Errorf("create table for %s: %w", c.Name, err)
		}
		progress[n] = migrate.TableProgress{Schema: targetSchema, Table: c.Name, EstRows: c.EstDocs}
		r.st.Tables[n] = c.Name
	}
	r.rep.ResetTables(progress)
	r.rep.Event("info", fmt.Sprintf("%s to %s: created %d table(s)", r.st.SourceVersion, r.st.TargetVersion, len(r.list)))
	r.st.SchemaDone = true
	r.save()
	return nil
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

// finalize runs once the stream has drained at cutover, or straight after
// the copy in an offline job. It returns false when validation failed and
// streaming should continue; an offline job has no stream to go back to and
// fails instead.
func (r *runner) finalize(ctx context.Context) (bool, error) {
	r.rep.SetPhase(migrate.PhaseCutover)
	offline := r.cfg.Offline()
	if offline {
		r.rep.Event("info", "validating collections ("+r.cfg.Validation+")")
	} else {
		r.rep.Event("info", "the stream is drained; validating collections ("+r.cfg.Validation+")")
	}
	// Collections created while the job ran are validated too. An offline
	// job is answerable only for the collections it copied.
	list := r.list
	if !offline {
		var err error
		if list, err = r.collections(ctx); err != nil {
			return false, err
		}
	}
	mismatches, err := validate(ctx, r.db, r.dst, list, r.cfg.Validation, r.rep)
	if err != nil || mismatches > 0 {
		why := fmt.Sprintf("%d collection(s) differ between source and target", mismatches)
		if err != nil {
			why = err.Error()
		}
		if offline {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("%s: the source was probably written to during the copy. Stop writes to the source and run the migration again", why)
		}
		r.rep.SetDesired("run")
		r.rep.Event("error", "cutover cancelled: "+why+". If the source is still receiving writes, stop them and try again; streaming continues")
		r.save()
		return false, nil
	}
	if _, err := r.dst.Exec(ctx, "ANALYZE"); err != nil {
		r.rep.Event("warn", "ANALYZE on the target failed: "+err.Error())
	}
	r.cleanup(ctx)
	r.rep.SetPhase(migrate.PhaseDone)
	if offline {
		r.rep.Event("info", "offline copy complete: the target holds the source's documents as of the copy")
	} else {
		r.rep.Event("info", "cutover complete: the target has every document of the source")
	}
	r.save()
	return true, nil
}

// cleanup removes the checkpoint from the target.
func (r *runner) cleanup(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	dst, err := postgres.Connect(ctx, r.dstCfg)
	if err != nil {
		r.rep.Event("warn", "could not connect to the target to remove the checkpoint: "+err.Error())
		return
	}
	defer dst.Close(ctx)
	var exists bool
	_ = dst.QueryRow(ctx, `SELECT to_regclass('`+metaSchema+`.mongo_checkpoint') IS NOT NULL`).Scan(&exists)
	if !exists {
		return
	}
	dst.Exec(ctx, `DELETE FROM `+metaSchema+`.mongo_checkpoint WHERE job_id = $1`, r.jobID)
	var left int
	if dst.QueryRow(ctx, `SELECT count(*) FROM `+metaSchema+`.mongo_checkpoint`).Scan(&left) == nil && left == 0 {
		dst.Exec(ctx, `DROP TABLE `+metaSchema+`.mongo_checkpoint`)
	}
}

func (r *runner) abort(ctx context.Context) migrate.Outcome {
	r.cleanup(ctx)
	r.rep.Event("info", "aborted. Tables already copied to the target were left in place")
	r.save()
	return migrate.Aborted
}
