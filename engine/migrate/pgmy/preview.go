package pgmy

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: the
// translated tables, then the indexes and foreign keys that are built once
// the rows are in. It reads the source catalog and looks in the target
// database for what is already there, exactly as the schema phase does,
// and takes the statements from the same plan, so what is shown is what a
// job started now would send.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, session settings,
// emptying the tables before a copy is repeated, the AUTO_INCREMENT
// counters set at cutover, which are not known before then, and ANALYZE.
// Nothing else is created at cutover.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close(context.WithoutCancel(ctx))
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()

	r := &runner{cfg: cfg, st: &pgpg.State{}, src: src, dst: dst}
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	found, err := mymy.FindExisting(ctx, dst, r.plan.objects())
	if err != nil {
		return nil, err
	}
	var skipped []string
	skipped, r.st.KeptObjects, r.st.DropFirst = mymy.Decide(cfg.Existing(), found)
	r.st.SkippedTables = r.plan.sources(skipped)
	if len(r.st.SkippedTables)+len(r.st.KeptObjects) > 0 {
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
		if len(r.plan.Tables) == 0 {
			return nil, pgpg.ErrNothingLeft
		}
	}

	out := mymy.DropStatements(found, r.st.DropFirst)
	out = appendStatements(out, migrate.PhaseSchema, r.plan.Pre)
	return appendStatements(out, migrate.PhaseIndexing, r.plan.Post), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
