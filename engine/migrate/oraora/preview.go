package oraora

import (
	"context"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

func (pipeline) Preview(ctx context.Context, src, dst connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	return Preview(ctx, src, dst, cfg)
}

// Preview returns the statements a job with these options would run on the
// target, in the order it would run them, without running any: sequences
// and tables first; then, once the rows are in, stored code, views and
// synonyms, and the indexes, constraints and comments over the finished
// tables; and the foreign keys and triggers, which are added at the very
// end. The definitions are the ones the source writes for its own objects,
// cleared of what belongs to the old server and rewritten for the schema
// that receives them, and they come from the same plan a job builds, so
// what is shown is what a job started now would send.
//
// It looks on the target for what is already there, as the schema phase
// does: what is to be kept is left out, and what is to be replaced is
// dropped first. Left out is what a job runs for its own bookkeeping, which
// is not part of the schema being migrated: the checkpoint table, emptying
// the tables before a copy is repeated, the final positions of sequences
// and identity columns, which are not known before then, recompiling and
// gathering statistics. Nothing is switched on at the source, which a job
// that follows changes does for the keys it needs logged.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := oracle.Open(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()

	plan, err := BuildPlan(ctx, src, dst, origin(srcCfg), cfg, kept{}, nil)
	if err != nil {
		return nil, err
	}
	if len(plan.MapProblems) > 0 {
		return nil, fmt.Errorf("schema mapping: %s", strings.Join(plan.MapProblems, "; "))
	}
	clashes, err := findClashes(ctx, dst, plan)
	if err != nil {
		return nil, err
	}

	var out []migrate.Statement
	switch {
	case len(clashes) == 0:
	case cfg.Existing() == migrate.ExistingSkip:
		st := &State{}
		st.keep(clashes, plan)
		if plan, err = BuildPlan(ctx, src, dst, origin(srcCfg), cfg, keptOf(st), plan); err != nil {
			return nil, err
		}
	case cfg.Existing() == migrate.ExistingReplace:
		if out, err = dropPreview(ctx, dst, clashes); err != nil {
			return nil, err
		}
	}

	out = appendStatements(out, migrate.PhaseSchema, plan.Pre)
	out = appendStatements(out, migrate.PhaseIndexing, plan.Code)
	out = appendStatements(out, migrate.PhaseIndexing, plan.Post)
	return appendStatements(out, migrate.PhaseCutover, plan.Final), nil
}

func appendStatements(out []migrate.Statement, phase string, list []Stmt) []migrate.Statement {
	for _, s := range list {
		out = append(out, migrate.Statement{Phase: phase, Kind: s.Kind, Name: s.Label(), SQL: s.SQL, End: scriptEnd(s)})
	}
	return out
}

// scriptEnd is what closes a statement in a script. A program unit ends
// in a semicolon of its own, which is part of it, and a script marks its
// end with a slash on a line by itself.
func scriptEnd(s Stmt) string {
	switch s.Kind {
	case "function", "procedure", "package", "package body", "type", "type body":
		return "/"
	case "trigger":
		// The statement that enables or disables it afterwards is plain.
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s.SQL)), "CREATE") {
			return "/"
		}
	}
	return ""
}

// dropPreview writes out the statements that remove what a job replaces,
// in the order dropClashes runs them. An index or a trigger on a table
// that is dropped goes with its table; a job finds it gone and moves on,
// and the preview leaves its statement out, so that the script runs as
// it is written.
func dropPreview(ctx context.Context, dst oracle.Queryer, clashes []clash) ([]migrate.Statement, error) {
	dropped := map[string]bool{}
	var schemas []string
	seen := map[string]bool{}
	for _, c := range clashes {
		if c.Held == "table" {
			dropped[c.label()] = true
		}
		if !seen[c.Schema] {
			seen[c.Schema] = true
			schemas = append(schemas, literal(c.Schema))
		}
	}
	in := strings.Join(schemas, ", ")
	rows, err := queryText(ctx, dst, `SELECT 'index', owner, index_name, table_owner, table_name FROM all_indexes WHERE owner IN (`+in+`)
		UNION ALL SELECT 'trigger', owner, trigger_name, table_owner, table_name FROM all_triggers WHERE owner IN (`+in+`) AND table_name IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	gone := map[string]bool{}
	for _, r := range rows {
		if dropped[r[3]+"."+r[4]] {
			gone[r[0]+" "+r[1]+"."+r[2]] = true
		}
	}

	var out []migrate.Statement
	for _, tablesTurn := range []bool{true, false} {
		for _, c := range clashes {
			if (c.Held == "table") != tablesTurn || gone[c.Held+" "+c.label()] {
				continue
			}
			out = append(out, migrate.Statement{Phase: migrate.PhaseSchema, Kind: "drop", Name: c.label(), SQL: dropStatement(c.Held, c.Schema, c.Name)})
		}
	}
	return out, nil
}
