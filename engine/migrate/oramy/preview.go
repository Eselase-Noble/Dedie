package oramy

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: the
// translated tables first, then the indexes and check constraints that
// are built once the rows are in, and the foreign keys, which are added at
// the very end. It reads the source catalog, asks the target for its
// release, on which the translation depends, and looks there for what
// already exists, exactly as the schema phase does. The statements come
// from the same plan, so what is shown is what a job started now would
// send.
//
// Left out is what a job runs for its own bookkeeping, which is not part
// of the schema being migrated: the checkpoint table, session settings,
// emptying the tables before a copy is repeated, the AUTO_INCREMENT
// counters set at the end, which are not known before then, and ANALYZE.
// A job that replaces what is there drops it with foreign key checks off,
// so that a table other tables refer to can go. Nothing is switched on at
// the source, which a job that follows changes does for the keys it needs
// logged.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := oracle.Open(ctx, srcCfg)
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
	version, err := targetVersion(ctx, dst)
	if err != nil {
		return nil, err
	}
	r.targetVersion = mysql.VersionNum(version)
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}

	var out []migrate.Statement
	if policy := cfg.Existing(); policy != migrate.ExistingFail {
		objects, err := existing(ctx, dst, r.plan)
		if err != nil {
			return nil, err
		}
		if policy == migrate.ExistingReplace {
			for _, f := range objects {
				out = append(out, migrate.Statement{Phase: migrate.PhaseSchema, Kind: "drop", Name: f.String(), SQL: f.drop()})
			}
		} else if len(objects) > 0 {
			if err := r.keepExisting(objects); err != nil {
				return nil, err
			}
			if err := r.buildPlan(ctx); err != nil {
				return nil, err
			}
		}
	}

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
