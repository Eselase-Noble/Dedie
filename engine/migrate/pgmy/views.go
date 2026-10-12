package pgmy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/viewsql"
)

// loadViews reads the views of the schemas in scope: the query of each as
// PostgreSQL prints it, and its columns. The connection's search_path is
// empty, as the PostgreSQL planner left it, so every table in a query is
// written with its schema. Views that belong to an extension are not the
// database's own and are left alone.
func loadViews(ctx context.Context, src *pgx.Conn, schemas []string) ([]*viewsql.View, error) {
	if _, err := src.Exec(ctx, `SELECT pg_catalog.set_config('search_path', '', false)`); err != nil {
		return nil, err
	}
	rows, err := src.Query(ctx, `SELECT c.oid, n.nspname, c.relname, coalesce(pg_catalog.pg_get_viewdef(c.oid), '')
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'v' AND n.nspname = ANY($1)
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend x
		                  WHERE x.classid = 'pg_catalog.pg_class'::regclass AND x.objid = c.oid AND x.deptype = 'e')
		ORDER BY 2, 3`, schemas)
	if err != nil {
		return nil, err
	}
	var views []*viewsql.View
	var oids []uint32
	byOID := map[uint32]*viewsql.View{}
	for rows.Next() {
		var oid uint32
		v := &viewsql.View{}
		if err := rows.Scan(&oid, &v.Schema, &v.Name, &v.Definition); err != nil {
			rows.Close()
			return nil, err
		}
		views, oids = append(views, v), append(oids, oid)
		byOID[oid] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(views) == 0 {
		return nil, err
	}
	// A column of a domain is classified by the type underneath.
	rows, err = src.Query(ctx, `SELECT a.attrelid, a.attname, b.typname, b.typcategory::text
		FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		JOIN pg_catalog.pg_type b ON b.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END
		WHERE a.attrelid = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name, typ, category string
		if err := rows.Scan(&oid, &name, &typ, &category); err != nil {
			return nil, err
		}
		v := byOID[oid]
		v.Columns = append(v.Columns, viewsql.Column{Name: name, Class: viewsql.PostgresClass(typ, category)})
	}
	return views, rows.Err()
}

// planViews translates the views. A view is named on the target by the
// rule that names the tables, and shares their one namespace there, so
// its name may turn out to be taken. A migration of hand-picked tables
// takes a view along only when everything it reads is among them; the
// rest are none of its business, and are not reported as left out.
func (p *Plan) planViews(views []*viewsql.View, tableScope bool) {
	// MySQL does not add to a date with +, and the translator has to be
	// told which names are dates to leave such views out.
	dates := map[string]bool{}
	for _, t := range p.Tables {
		for _, c := range t.Columns {
			if strings.HasPrefix(c.Type, "DATE") || strings.HasPrefix(c.Type, "TIME") {
				dates[strings.ToLower(c.Name)] = true
			}
		}
	}
	p.Views = viewsql.Plan(views, viewsql.Naming{From: viewsql.Postgres, To: viewsql.MySQL,
		Ident:    func(name string) string { return name },
		Temporal: func(column string) bool { return dates[strings.ToLower(column)] },
		Table: func(schema, name string) (viewsql.Relation, bool) {
			t := p.table(schema, name)
			if t == nil {
				return viewsql.Relation{}, false
			}
			return viewsql.Relation{Quoted: t.QName(), Bare: t.Target}, true
		},
		View: func(schema, name string) (string, viewsql.Relation, string) {
			target := p.targetName(schema, name)
			reason := ""
			if other, taken := p.claimed[strings.ToLower(target)]; taken {
				reason = fmt.Sprintf("its name on the target would be %s, which is the name %s gets there", target, other)
			} else {
				p.claimed[strings.ToLower(target)] = schema + "." + name
			}
			return target, viewsql.Relation{Quoted: mysql.Quote(target), Bare: target}, reason
		}})
	if tableScope {
		p.Views = viewsql.Kept(p.Views)
	}
}

// viewChecker says how this pipeline reaches its two databases to create
// the translated views and compare them with the source's.
func (r *runner) viewChecker() viewsql.Checker {
	return viewsql.Checker{From: viewsql.Postgres, To: viewsql.MySQL,
		Ident: func(name string) string { return name },
		SourceRelations: func(v *viewsql.View) []string {
			return []string{pgx.Identifier{v.Schema, v.Name}.Sanitize()}
		},
		SourceRow: func(ctx context.Context, query string) ([]*string, error) {
			// A query that ran out of time takes its connection with it.
			if err := r.reconnect(context.WithoutCancel(ctx)); err != nil {
				return nil, err
			}
			return pgpg.TextRow(ctx, r.src, query)
		},
		TargetRow: func(ctx context.Context, query string) ([]*string, error) {
			row, err := viewsql.Row(ctx, r.dst, query)
			if err != nil {
				err = errors.New(message(err))
			}
			return row, err
		},
		Create: func(ctx context.Context, v *viewsql.View) (bool, error) {
			_, err := r.dst.ExecContext(ctx, v.SQL)
			switch {
			case err == nil:
				return false, nil
			case alreadyExists(err):
				return true, nil
			}
			return false, errors.New(message(err))
		},
		Drop: func(ctx context.Context, v *viewsql.View) error {
			if _, err := r.dst.ExecContext(ctx, "DROP VIEW IF EXISTS "+v.Target.Quoted); err != nil {
				return errors.New(message(err))
			}
			return nil
		},
		TargetColumns: func(ctx context.Context, v *viewsql.View) ([]viewsql.Column, error) {
			return mymy.ViewColumns(ctx, r.dst, v.Target.Bare)
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
