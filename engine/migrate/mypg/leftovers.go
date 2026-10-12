package mypg

import (
	"context"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/viewsql"
)

// Why each kind of object stays behind.
var leftoverReasons = map[string]string{
	"procedure": "stored procedures are written in MySQL's own procedural language, which is not translated",
	"function":  "stored functions are written in MySQL's own procedural language, which is not translated",
	"trigger":   "triggers are written in MySQL's own procedural language, which is not translated",
	"event":     "PostgreSQL has no scheduled events; this needs a job in a scheduler such as pg_cron",
}

const noDefinition = "; the source account may not read its definition, so it is not shown here"

// Leftovers lists what a job with these options does not migrate, with
// the statement that defines each object on the source: the views that
// could not be translated or did not survive verification, and stored
// routines, triggers and scheduled events. The definitions are MySQL's
// own, without the DEFINER and the database's name, as the MySQL to MySQL
// pipeline would create them. It changes neither database.
func Leftovers(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, st mymy.State) ([]migrate.Leftover, error) {
	cfg.Defaults()
	r, _, done, err := planned(ctx, srcCfg, dstCfg, cfg, &st)
	if err != nil {
		return nil, err
	}
	defer done()

	// The source plan already holds every definition, read for the
	// pipeline that copies them as they are.
	defined := map[string]string{}
	var out, code []migrate.Leftover
	for _, list := range [][]mymy.Stmt{r.plan.Plan.Post, r.plan.Plan.Final} {
		for _, s := range list {
			if s.Kind == "view" {
				defined[s.Name] = s.SQL
				continue
			}
			l := migrate.Leftover{Kind: s.Kind, Name: s.Name, Reason: leftoverReasons[s.Kind], Source: s.SQL}
			if s.SQL == "" {
				l.Reason += noDefinition
			}
			code = append(code, l)
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
	return append(out, code...), nil
}
