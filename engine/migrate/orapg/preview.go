package orapg

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: schemas,
// sequences and the translated tables first, then the indexes and check
// constraints that are built once the rows are in, and the foreign keys,
// which are added at the very end. It reads the source catalog and looks
// on the target for what is already there, exactly as the schema phase
// does, and takes the statements from the same plan, so what is shown is
// what a job started now would send.
//
// A sequence is shown starting where the source's stands now; a job
// writes the position of the moment it starts, and sets it again at the
// end. Left out is what a job runs for its own bookkeeping, which is not
// part of the schema being migrated: the checkpoint table, emptying the
// tables before a copy is repeated, those final positions of sequences
// and identity columns, which are not known before then, and ANALYZE.
// Nothing is switched on at the source, which a job that follows changes
// does for the keys it needs logged.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(context.WithoutCancel(ctx))

	r := &runner{cfg: cfg, st: &State{}, srcCfg: srcCfg, src: src, dst: dst}
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	found, err := pgpg.FindExisting(ctx, dst, r.plan.claims())
	if err != nil {
		return nil, err
	}
	var skipped []string
	skipped, r.st.KeptObjects, r.st.DropFirst = pgpg.Decide(cfg.Existing(), found)
	r.st.SkippedTables = r.plan.sources(skipped)
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
	out = appendStatements(out, migrate.PhaseIndexing, r.plan.Post)
	return appendStatements(out, migrate.PhaseCutover, r.plan.Final), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
