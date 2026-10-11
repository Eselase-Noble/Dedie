package pgpg

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any. It reads
// the source catalog and looks on the target for what is already there,
// exactly as the schema phase does, and takes the statements from the same
// plan, so what is shown is what a job started now would send.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, session settings,
// emptying the tables before a copy is repeated, the positions sequences
// are set to at cutover, which are not known before then, and ANALYZE.
//
// A statement the job may retry is listed once, in the phase that first
// runs it. Whether foreign keys are built with the indexes or wait for
// cutover depends on the target account; see indexingPhase.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close(context.WithoutCancel(ctx))
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(context.WithoutCancel(ctx))

	r := &runner{cfg: cfg, st: &State{}, src: src, dst: dst}
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	found, err := FindExisting(ctx, dst, r.plan.claims())
	if err != nil {
		return nil, err
	}
	r.st.SkippedTables, r.st.KeptObjects, r.st.DropFirst = Decide(cfg.Existing(), found)
	if len(r.st.SkippedTables)+len(r.st.KeptObjects) > 0 {
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
		if len(r.plan.Tables) == 0 {
			return nil, ErrNothingLeft
		}
	}

	indexing, cutover := r.plan.deferred(cfg.Offline() || replicaRole(ctx, dst))
	out := DropStatements(found, r.st.DropFirst)
	out = appendStatements(out, PhaseSchema, r.plan.Pre)
	out = appendStatements(out, PhaseIndexing, indexing)
	return appendStatements(out, PhaseCutover, cutover), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
