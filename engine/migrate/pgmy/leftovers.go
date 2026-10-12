package pgmy

import (
	"context"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/viewsql"
)

// Why each kind of object stays behind.
var leftoverReasons = map[string]string{
	"function":             "functions and procedures are written in PostgreSQL's own languages, which are not translated",
	"trigger":              "a trigger calls a PostgreSQL function, and those are not translated",
	"materialized view":    "MySQL has no materialized views; this needs a table and something to refresh it",
	"check constraint":     "check constraints are written in PostgreSQL's dialect and are not translated",
	"exclusion constraint": "MySQL has no exclusion constraints",
}

// Leftovers lists what a job with these options does not migrate, with
// the statement that defines each object on the source: the views that
// could not be translated or did not survive verification, and the
// functions, triggers, materialized views and check constraints. The
// definitions are PostgreSQL's own, as its catalog functions print them
// and as the PostgreSQL to PostgreSQL pipeline would run them. It changes
// neither database.
func Leftovers(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st pgpg.State) ([]migrate.Leftover, error) {
	cfg.Defaults()
	r, _, done, err := planned(ctx, srcCfg, dstCfg, cfg, &st)
	if err != nil {
		return nil, err
	}
	defer done()

	dropped := map[string]string{}
	for _, res := range st.Views {
		if res.Status == viewsql.Dropped {
			dropped[res.Name] = res.Detail
		}
	}
	var out []migrate.Leftover
	for _, v := range r.plan.Views {
		reason := v.Reason
		if detail, was := dropped[v.Source()]; was {
			reason = "it was translated, but not kept on the target: " + detail
		} else if v.SQL != "" {
			continue
		}
		l := migrate.Leftover{Kind: "view", Name: v.Source(), Reason: reason}
		if v.Definition != "" {
			l.Source = "CREATE VIEW " + pgx.Identifier{v.Schema, v.Name}.Sanitize() + " AS\n" + v.Definition
		}
		out = append(out, l)
	}
	// The source plan already holds the other definitions, read for the
	// pipeline that copies them as they are.
	for _, list := range [][]pgpg.Stmt{r.plan.Plan.Pre, r.plan.Plan.Post, r.plan.Plan.Final} {
		for _, s := range list {
			if reason, ok := leftoverReasons[s.Kind]; ok {
				out = append(out, migrate.Leftover{Kind: s.Kind, Name: s.Name, Reason: reason, Source: s.SQL})
			}
		}
	}
	return out, nil
}
