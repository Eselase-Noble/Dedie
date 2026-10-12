package mypg

import (
	"context"
	"fmt"
	"strings"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/viewsql"
)

// loadViews reads the views of the source database: the query of each as
// MySQL stores it, with every name quoted and qualified, and its columns.
// The query is empty when the account may not see the definition.
func loadViews(ctx context.Context, src mymy.Querier, database string) ([]*viewsql.View, error) {
	rows, err := mymy.QueryStrings(ctx, src, `SELECT table_name, COALESCE(view_definition, '') FROM information_schema.views
		WHERE table_schema = ? ORDER BY 1`, database)
	if err != nil {
		return nil, err
	}
	var views []*viewsql.View
	byName := map[string]*viewsql.View{}
	for _, r := range rows {
		v := &viewsql.View{Schema: database, Name: r[0], Definition: r[1]}
		views = append(views, v)
		byName[v.Name] = v
	}
	if len(views) == 0 {
		return nil, nil
	}
	rows, err = mymy.QueryStrings(ctx, src, `SELECT c.table_name, c.column_name, c.data_type FROM information_schema.columns c
		JOIN information_schema.views v ON v.table_schema = c.table_schema AND v.table_name = c.table_name
		WHERE c.table_schema = ? ORDER BY c.table_name, c.ordinal_position`, database)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if v := byName[r[0]]; v != nil {
			v.Columns = append(v.Columns, viewsql.Column{Name: r[1], Class: viewsql.MySQLClass(r[2])})
		}
	}
	return views, nil
}

// planViews translates the views. They keep their names and go where the
// tables go. A migration of hand-picked tables takes a view along only
// when everything it reads is among them; the rest are none of its
// business, and are not reported as left out.
func (p *Plan) planViews(views []*viewsql.View, tableScope bool) {
	place := func(name string) viewsql.Relation { return viewsql.Relation{Quoted: qname(name), Bare: name} }
	p.Views = viewsql.Plan(views, viewsql.Naming{From: viewsql.MySQL, To: viewsql.Postgres,
		Ident: func(name string) string { return name },
		Table: func(schema, name string) (viewsql.Relation, bool) {
			return place(name), strings.EqualFold(schema, p.Database) && p.Table(name) != nil
		},
		View: func(_, name string) (string, viewsql.Relation, string) {
			reason := ""
			if len(name) > 63 {
				reason = "its name is longer than the 63 bytes PostgreSQL allows"
			}
			return name, place(name), reason
		}})
	if tableScope {
		p.Views = viewsql.Kept(p.Views)
	}
}

// viewChecker says how this pipeline reaches its two databases to create
// the translated views and compare them with the source's.
func (r *runner) viewChecker() viewsql.Checker {
	return viewsql.Checker{From: viewsql.MySQL, To: viewsql.Postgres,
		Ident:           func(name string) string { return name },
		SourceRelations: func(v *viewsql.View) []string { return []string{mysql.Quote(v.Name)} },
		SourceRow: func(ctx context.Context, query string) ([]*string, error) {
			return viewsql.Row(ctx, r.src, query)
		},
		TargetRow: func(ctx context.Context, query string) ([]*string, error) {
			row, err := pgpg.TextRow(ctx, r.dst, query)
			if err != nil {
				err = fmt.Errorf("%s", pgMessage(err))
			}
			return row, err
		},
		Create: func(ctx context.Context, v *viewsql.View) (bool, error) {
			// A query that ran out of time takes its connection with it.
			if err := r.reconnect(ctx); err != nil {
				return false, err
			}
			_, err := r.dst.PgConn().Exec(ctx, v.SQL).ReadAll()
			switch {
			case err == nil:
				return false, nil
			case alreadyExists(err):
				return true, nil
			}
			return false, fmt.Errorf("%s", pgMessage(err))
		},
		Drop: func(ctx context.Context, v *viewsql.View) error {
			if err := r.reconnect(ctx); err != nil {
				return err
			}
			if _, err := r.dst.Exec(ctx, "DROP VIEW IF EXISTS "+v.Target.Quoted); err != nil {
				return fmt.Errorf("%s", pgMessage(err))
			}
			return nil
		},
		TargetColumns: func(ctx context.Context, v *viewsql.View) ([]viewsql.Column, error) {
			return pgpg.ViewColumns(ctx, r.dst, v.Target.Quoted)
		},
		Event:    r.rep.Event,
		Previous: r.st.Views,
		Save: func(results []viewsql.Result) {
			r.st.Views = results
			r.save()
		},
	}
}

// finishViews creates the translated views and keeps those that return
// the same as on the source. It runs when the tables have been validated,
// which is the one moment both databases are known to hold the same rows.
// What becomes of each view is saved in the job's state as it goes.
func (r *runner) finishViews(ctx context.Context) error {
	n := len(viewsql.Kept(r.plan.Views))
	if n == 0 {
		return nil
	}
	r.rep.Event("info", fmt.Sprintf("creating %d translated view(s) and comparing each with the source", n))
	_, err := r.viewChecker().Finish(ctx, r.plan.Views)
	return err
}
