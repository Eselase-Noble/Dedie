package mypg

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
// the rows are in. It reads the source catalog and looks on the target for
// what is already there, exactly as the schema phase does, and takes the
// statements from the same plan, so what is shown is what a job started
// now would send.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, emptying the tables
// before a copy is repeated, the positions identity columns are set to at
// cutover, which are not known before then, and ANALYZE. Nothing else is
// created at cutover.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(context.WithoutCancel(ctx))

	r := &runner{cfg: cfg, st: &mymy.State{}, srcCfg: srcCfg, src: src, dst: dst}
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	found, err := pgpg.FindExisting(ctx, dst, r.plan.claims())
	if err != nil {
		return nil, err
	}
	r.st.SkippedTables, r.st.KeptObjects, r.st.DropFirst = pgpg.Decide(cfg.Existing(), found)
	if len(r.st.SkippedTables)+len(r.st.KeptObjects) > 0 {
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
		if len(r.plan.Tables) == 0 {
			return nil, pgpg.ErrNothingLeft
		}
	}

	out := pgpg.DropStatements(found, r.st.DropFirst)
	out = appendStatements(out, migrate.PhaseSchema, r.plan.Pre)
	return appendStatements(out, migrate.PhaseIndexing, r.plan.Post), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
