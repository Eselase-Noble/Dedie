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
	// keyless remembers the tables already reported as not followed.
	keyless    map[string]bool
	undoWarned bool
}

// Run drives a job from wherever its saved state left it to completion,
// or until the operator pauses or aborts it. It is safe to call again
// after a crash: finished phases are skipped and the stream resumes from
// the checkpoint stored on the target.
func Run(ctx context.Context, jobID string, cfg migrate.Config, st State, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	cfg.Defaults()
	r := &runner{jobID: jobID, cfg: cfg, st: &st, srcCfg: srcCfg, dstCfg: dstCfg, rep: rep, keyless: map[string]bool{}}
	return r.run(ctx)
}

func (r *runner) save() { r.rep.SaveState(*r.st) }

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
			r.rep.Event("info", "paused; the job can resume for as long as the source still has its redo logs from "+r.st.AppliedLSN+" onwards")
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
	// A retry after an interrupted schema phase finds its own tables on
	// the target; that is the one failed check it may ignore.
	var ours bool
	_ = r.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+metaSchema+`.oracle_checkpoint WHERE job_id = $1)`, r.jobID).Scan(&ours)
	var blocking []string
	for _, c := range report.Checks {
		if c.Status == "fail" && !(ours && c.Name == "Target is empty") {
			blocking = append(blocking, c.Name+": "+c.Detail)
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("preflight failed: %s", strings.Join(blocking, "; "))
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

// logKeys makes the redo stream carry the primary key of every changed
// row, for the tables where it does not already. Without the key a
// change cannot be matched to its row on the target. It must happen
// before the copy is taken, so that every change after it is covered.
func (r *runner) logKeys(ctx context.Context) error {
	logged, everywhere := keyLogged(ctx, r.src, r.plan)
	if everywhere {
		return nil
	}
	conn, err := r.src.Conn(ctx)
	if err != nil {
		return oracle.Friendly(err)
	}
	defer conn.Close()
	// Wait briefly for transactions in flight instead of failing at once.
	conn.ExecContext(ctx, `ALTER SESSION SET DDL_LOCK_TIMEOUT = 30`)
	added := 0
	for _, t := range r.plan.Tables {
		if len(t.PK) == 0 || logged[t.Label()] {
			continue
		}
		if _, err := conn.ExecContext(ctx, "ALTER TABLE "+t.Source()+" ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS"); err != nil {
			if strings.Contains(err.Error(), "ORA-00054") {
				return fmt.Errorf("switch on primary key logging for %s: a transaction has held the table for more than 30 seconds, and the change needs it for an instant. "+
					"Start the job again when that transaction has ended, or switch the logging on yourself beforehand with ALTER TABLE ... ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS", t.Label())
			}
			return fmt.Errorf("switch on primary key logging for %s: %w", t.Label(), oracle.Friendly(err))
		}
		r.st.LogGroups = append(r.st.LogGroups, t.Label())
		r.save()
		added++
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
	conn, err := r.src.Conn(ctx)
	if err != nil {
		r.rep.Event("warn", "could not connect to the source to switch primary key logging off again: "+oracle.Friendly(err).Error())
		return
	}
	defer conn.Close()
	conn.ExecContext(ctx, `ALTER SESSION SET DDL_LOCK_TIMEOUT = 30`)
	var left []string
	for _, label := range r.st.LogGroups {
		owner, name, _ := strings.Cut(label, ".")
		_, err := conn.ExecContext(ctx, "ALTER TABLE "+oracle.QName(owner, name)+" DROP SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS")
		// ORA-32587: the log group is already gone; ORA-00942: so is the table.
		if err != nil && !strings.Contains(err.Error(), "ORA-32587") && !strings.Contains(err.Error(), "ORA-00942") {
			left = append(left, label)
		}
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

	// The redo stream is read from before the snapshot, far enough back
	// to include the first change of every transaction still open when
	// the snapshot is taken. The order of these three reads is what
	// guarantees it: a transaction open at the snapshot either began
	// before the first read, and is then seen by the second, or began
	// after it.
	before, err := currentSCN(ctx, r.src)
	if err != nil {
		return err
	}
	oldest := oldestOpen(ctx, r.src)
	scn, err := settle(ctx, r.src, r.plan)
	if err != nil {
		return err
	}
	r.st.SnapshotSCN, r.st.MineFrom = scn, before
	if oldest > 0 && oldest < before {
		r.st.MineFrom = oldest
	}
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
	owners := make([]string, len(r.plan.Owners))
	for i, o := range r.plan.Owners {
		owners[i] = literal(o)
	}
	// LAST_NUMBER is at or beyond every value handed out, cached ones included.
	rows, err := query(ctx, r.src, `SELECT sequence_owner, sequence_name, TO_CHAR(last_number) FROM all_sequences
		WHERE sequence_owner IN (`+strings.Join(owners, ", ")+`)`)
	if err != nil {
		return 0, err
	}
	next := map[string]string{}
	for _, row := range rows {
		next[row[0]+"."+row[1]] = row[2]
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
