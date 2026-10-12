package pgmy

import (
	"context"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/storedcode"
)

// Why each kind of object stays behind when no conversion is tried.
var leftoverReasons = map[string]string{
	"function":             "it is written in one of PostgreSQL's own languages",
	"trigger":              "its code is a function in one of PostgreSQL's own languages",
	"materialized view":    "MySQL has no materialized views; this needs a table and something to refresh it",
	"check constraint":     "check constraints are written in PostgreSQL's dialect and are not translated",
	"exclusion constraint": "MySQL has no exclusion constraints",
}

// Leftovers lists what a job with these options does not migrate, with
// the statement that defines each object on the source: the views that
// could not be translated or did not survive verification, the functions,
// procedures and triggers the job does not create or created with a
// caveat, with the converted draft where there is one, and the
// materialized views and check constraints. The definitions are
// PostgreSQL's own, as its catalog functions print them. It changes
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
		if res.Lost() {
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
	return append(out, storedcode.Leftovers(mysql.Kind, r.plan.Code, st.Code)...), nil
}
