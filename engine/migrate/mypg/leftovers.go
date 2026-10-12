package mypg

import (
	"context"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

// Why each kind of object stays behind when no conversion is tried.
var leftoverReasons = map[string]string{
	"procedure": "it is written in MySQL's own procedural language",
	"function":  "it is written in MySQL's own procedural language",
	"trigger":   "it is written in MySQL's own procedural language",
	"event":     "PostgreSQL has no scheduled events; this needs a job in a scheduler such as pg_cron",
}

const noDefinition = "; the source account may not read its definition, so it is not shown here"

// Leftovers lists what a job with these options does not migrate, with
// the statement that defines each object on the source: the views that
// could not be translated or did not survive verification, the stored
// routines and triggers the job does not create or created with a caveat,
// with the converted draft where there is one, and scheduled events. The
// definitions are MySQL's own, without the DEFINER and the database's
// name, as the MySQL to MySQL pipeline would create them. It changes
// neither database.
func Leftovers(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st mymy.State) ([]migrate.Leftover, error) {
	cfg.Defaults()
	r, _, done, err := planned(ctx, srcCfg, dstCfg, cfg, &st)
	if err != nil {
		return nil, err
	}
	defer done()

	// The source plan holds each view's definition without its DEFINER
	// and the database's name.
	defined := map[string]string{}
	var out []migrate.Leftover
	for _, s := range r.plan.Plan.Post {
		if s.Kind == "view" {
			defined[s.Name] = s.SQL
		}
	}
	view := func(v *viewsql.View, reason string) {
		source := defined[v.Name]
		if source == "" && v.Definition != "" {
			source = "CREATE VIEW " + mysql.Quote(v.Name) + " AS " + v.Definition
		}
		if source == "" {
			reason += noDefinition
		}
		out = append(out, migrate.Leftover{Kind: "view", Name: v.Name, Reason: reason, Source: source})
	}
	dropped := map[string]string{}
	for _, res := range st.Views {
		if res.Lost() {
			dropped[res.Name] = res.Detail
		}
	}
	for _, v := range r.plan.Views {
		if detail, was := dropped[v.Source()]; was {
			view(v, "it was translated, but not kept on the target: "+detail)
		} else if v.SQL == "" {
			view(v, v.Reason)
		}
	}
	return append(out, storedcode.Leftovers(postgres.Kind, r.plan.Code, st.Code)...), nil
}
