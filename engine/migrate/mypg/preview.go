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
	"dedie/engine/migrate/viewsql"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: the
// translated tables, then the indexes and foreign keys that are built once
// the rows are in, then the translated views. It reads the source catalog and looks on the target for
// what is already there, exactly as the schema phase does, and takes the
// statements from the same plan, so what is shown is what a job started
// now would send.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, emptying the tables
// before a copy is repeated, the positions identity columns are set to at
// cutover, which are not known before then, and ANALYZE.
//
// The translated views are shown at the end, where they are created. A
// job compares each with the source's view afterwards and drops one that
// does not return the same, so not every view shown is certain to stay.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	r, found, done, err := planned(ctx, srcCfg, dstCfg, cfg, &mymy.State{})
	if err != nil {
		return nil, err
	}
	defer done()

	out := pgpg.DropStatements(found, r.st.DropFirst)
	out = appendStatements(out, migrate.PhaseSchema, r.plan.Pre)
	out = appendStatements(out, migrate.PhaseIndexing, r.plan.Post)
	for _, v := range viewsql.Kept(r.plan.Views) {
		out = append(out, migrate.Statement{Phase: migrate.PhaseCutover, Kind: "view", Name: v.Label, SQL: v.SQL})
	}
	return out, nil
}

// planned connects to both databases and builds the plan a job with these
// options has after its schema phase, without running anything. For a job
// that has not got that far it looks on the target for what is already
// there and applies the policy chosen for it, exactly as the schema phase
// does; for one that has, the decision is in its state. The caller closes
// the connections with the function returned.
func planned(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st *mymy.State) (*runner, []pgpg.Existing, func(), error) {
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("source: %w", err)
	}
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		src.Close()
		return nil, nil, nil, fmt.Errorf("target: %w", err)
	}
	done := func() {
		dst.Close(context.WithoutCancel(ctx))
		src.Close()
	}
	r := &runner{cfg: cfg, st: st, srcCfg: srcCfg, src: src, dst: dst}
	found, err := r.settle(ctx)
	if err != nil {
		done()
		return nil, nil, nil, err
	}
	return r, found, done, nil
}

// settle builds the runner's plan with the decision on existing objects
// applied, and returns what was found on the target when it had to look.
func (r *runner) settle(ctx context.Context) ([]pgpg.Existing, error) {
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	if r.st.SchemaDone {
		return nil, nil
	}
	found, err := pgpg.FindExisting(ctx, r.dst, r.plan.claims())
	if err != nil {
		return nil, err
	}
	r.st.SkippedTables, r.st.KeptObjects, r.st.DropFirst = pgpg.Decide(r.cfg.Existing(), found)
	if len(r.st.SkippedTables)+len(r.st.KeptObjects) > 0 {
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
		if len(r.plan.Tables) == 0 {
			return nil, pgpg.ErrNothingLeft
		}
	}
	return found, nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
