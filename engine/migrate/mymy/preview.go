package mymy

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any. It reads
// the source catalog and looks in the target database for what is already
// there, exactly as the schema phase does, and takes the statements from
// the same plan, so what is shown is what a job started now would send.
//
// The tables come as the source writes them out, each with its indexes
// and foreign keys. They refer to each other in any order, which is why a
// job creates them on a session with foreign_key_checks off; anyone
// running the statements by hand needs the same.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, session settings,
// emptying the tables before a copy is repeated, the AUTO_INCREMENT
// counters set at cutover, which are not known before then, and ANALYZE.
// A view, routine, trigger or event whose definition the source account
// may not read is reported by the job and never run, so it is not listed.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()

	r := &runner{cfg: cfg, st: &State{}, srcCfg: srcCfg, src: src, dst: dst}
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	found, err := FindExisting(ctx, dst, r.plan.objects())
	if err != nil {
		return nil, err
	}
	r.st.SkippedTables, r.st.KeptObjects, r.st.DropFirst = Decide(cfg.Existing(), found)
	if len(r.st.SkippedTables)+len(r.st.KeptObjects) > 0 {
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
		if len(r.plan.Tables) == 0 {
			return nil, pgpg.ErrNothingLeft
		}
	}

	out := DropStatements(found, r.st.DropFirst)
	out = appendStatements(out, migrate.PhaseSchema, r.plan.Pre)
	out = appendStatements(out, migrate.PhaseIndexing, r.plan.Post)
	return appendStatements(out, migrate.PhaseCutover, r.plan.Final), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		if s.SQL != "" {
			out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
		}
	}
	return out
}
