package oramy

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: the
// translated tables first, then the indexes and check constraints that
// are built once the rows are in, and at the very end the foreign keys and
// the translated views. A job compares each view with the source's
// afterwards and drops one that does not return the same, so not every
// view shown is certain to stay. Last come the converted functions,
// procedures and triggers the job will create. It reads the source catalog, asks the target for its
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
	r, replaced, done, err := planned(ctx, srcCfg, dstCfg, cfg, &State{})
	if err != nil {
		return nil, err
	}
	defer done()

	var out []migrate.Statement
	for _, f := range replaced {
		out = append(out, migrate.Statement{Phase: migrate.PhaseSchema, Kind: "drop", Name: f.String(), SQL: f.drop()})
	}
	out = appendStatements(out, migrate.PhaseSchema, r.plan.Pre)
	out = appendStatements(out, migrate.PhaseIndexing, r.plan.Post)
	out = appendStatements(out, migrate.PhaseCutover, r.plan.Final)
	for _, v := range viewsql.Kept(r.plan.Views) {
		out = append(out, migrate.Statement{Phase: migrate.PhaseCutover, Kind: "view", Name: v.Label, SQL: v.SQL})
	}
	// A routine's body has semicolons inside, so a script has to end it
	// with something else.
	return append(out, storedcode.Statements(r.plan.Code, "//")...), nil
}

// planned connects to both databases and builds the plan a job with these
// options has after its schema phase, without running anything. For a job
// that has not settled what happens to the objects already on the target
// it looks for them and applies the policy chosen, as the schema phase
// does: what is to stay is left out of the plan, and what is to be
// replaced is returned for the caller to show. The caller closes the
// connections with the function returned.
func planned(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st *State) (r *runner, replaced []found, done func(), err error) {
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("source: %w", err)
	}
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		src.Close()
		return nil, nil, nil, fmt.Errorf("target: %w", err)
	}
	done = func() {
		dst.Close()
		src.Close()
	}
	r = &runner{cfg: cfg, st: st, srcCfg: srcCfg, src: src, dst: dst}
	if replaced, err = r.settle(ctx); err != nil {
		done()
		return nil, nil, nil, err
	}
	return r, replaced, done, nil
}

// settle builds the runner's plan with the decision on existing objects
// applied, and returns the objects a job would drop first.
func (r *runner) settle(ctx context.Context) ([]found, error) {
	version, err := targetVersion(ctx, r.dst)
	if err != nil {
		return nil, err
	}
	r.targetVersion = mysql.VersionNum(version)
	if err := r.buildPlan(ctx); err != nil {
		return nil, err
	}
	policy := r.cfg.Existing()
	if r.st.ExistingDecided || policy == migrate.ExistingFail {
		return nil, nil
	}
	objects, err := existing(ctx, r.dst, r.plan)
	if err != nil {
		return nil, err
	}
	if policy == migrate.ExistingReplace {
		return objects, nil
	}
	if len(objects) > 0 {
		if err := r.keepExisting(objects); err != nil {
			return nil, err
		}
		if err := r.buildPlan(ctx); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// Leftovers lists what a job with these options does not migrate, with
// the statement that defines each object on the source: the views that
// could not be translated or did not survive verification, the PL/SQL the
// job does not create or created with a caveat, with the converted draft
// where there is one, and packages, types and materialized views. It
// changes neither database.
func Leftovers(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st State) ([]migrate.Leftover, error) {
	cfg.Defaults()
	r, _, done, err := planned(ctx, srcCfg, dstCfg, cfg, &st)
	if err != nil {
		return nil, err
	}
	defer done()
	return oradef.Leftovers(mysql.Kind, r.plan.Views, st.Views, r.plan.Code, st.Code), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Name, SQL: s.SQL})
	}
	return out
}
