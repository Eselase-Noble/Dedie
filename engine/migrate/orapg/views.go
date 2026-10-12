package orapg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/viewsql"
)

// planViews translates the views. A view is named as the tables are: in
// the PostgreSQL schema of its Oracle schema, in lower case unless it was
// created with a quoted name. A migration of hand-picked tables takes a
// view along only when everything it reads is among them; the rest are
// none of its business, and are not reported as left out.
func (p *Plan) planViews(views []*viewsql.View, tableScope bool) {
	schemas := map[string]bool{}
	for _, t := range p.Tables {
		schemas[t.Owner] = true
	}
	p.Views = viewsql.Plan(views, viewsql.Naming{From: viewsql.Oracle, To: viewsql.Postgres, Ident: pgName,
		Table: func(owner, name string) (viewsql.Relation, bool) {
			t := p.Table(owner, name)
			if t == nil {
				return viewsql.Relation{}, false
			}
			return viewsql.Relation{Quoted: t.QName(), Bare: t.Target}, true
		},
		View: func(owner, name string) (string, viewsql.Relation, string) {
			schema, target := pgName(owner), pgName(name)
			reason := ""
			switch {
			case !schemas[owner]:
				reason = oradef.NoPlace(owner)
			case len(target) > 63:
				reason = "its name is longer than the 63 bytes PostgreSQL allows"
			}
			return schema + "." + target, viewsql.Relation{Quoted: pgx.Identifier{schema, target}.Sanitize(), Bare: target}, reason
		}})
	if tableScope {
		p.Views = viewsql.Kept(p.Views)
	}
}

// viewChecker says how this pipeline reaches its two databases to create
// the translated views and compare them with the source's.
func (r *runner) viewChecker() viewsql.Checker {
	return viewsql.Checker{From: viewsql.Oracle, To: viewsql.Postgres, Ident: pgName,
		SourceRelations: func(v *viewsql.View) []string {
			now := oracle.QName(v.Schema, v.Name)
			// A one-time copy holds the source as it was when copied, so
			// that is the moment the view is read at; if Oracle no longer
			// has it, the view is read as it is now.
			if r.cfg.Offline() && r.st.SnapshotSCN > 0 {
				return []string{fmt.Sprintf("%s AS OF SCN %d", now, r.st.SnapshotSCN), now}
			}
			return []string{now}
		},
		SourceRow: func(ctx context.Context, query string) ([]*string, error) {
			row, err := viewsql.Row(ctx, r.src, query)
			return row, oracle.Friendly(err)
		},
		TargetRow: func(ctx context.Context, query string) ([]*string, error) {
			row, err := pgpg.TextRow(ctx, r.dst, query)
			if err != nil {
				err = errors.New(pgMessage(err))
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
			return false, errors.New(pgMessage(err))
		},
		Drop: func(ctx context.Context, v *viewsql.View) error {
			if err := r.reconnect(ctx); err != nil {
				return err
			}
			if _, err := r.dst.Exec(ctx, "DROP VIEW IF EXISTS "+v.Target.Quoted); err != nil {
				return errors.New(pgMessage(err))
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
