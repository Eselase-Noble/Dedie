package oramy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/viewsql"
)

// planViews translates the views. A view is named as the tables are, in
// lower case unless it was created with a quoted name, and shares their
// one namespace in the target database, so its name may turn out to be
// taken. A migration of hand-picked tables takes a view along only when
// everything it reads is among them; the rest are none of its business,
// and are not reported as left out.
func (p *Plan) planViews(views []*viewsql.View, tableScope bool) {
	// MySQL does not tell the case of a table's name apart on every
	// server, so names are compared without it.
	taken := map[string]string{}
	for _, t := range p.Tables {
		taken[strings.ToLower(t.Target)] = "the table " + t.Label()
	}
	p.Views = viewsql.Plan(views, viewsql.Naming{From: viewsql.Oracle, To: viewsql.MySQL, Ident: myName,
		// MySQL does not add to a date with +, and the translator has to
		// be told which names are dates to leave such views out.
		Temporal: oradef.Dates(p.Catalog),
		Table: func(owner, name string) (viewsql.Relation, bool) {
			t := p.Table(owner, name)
			if t == nil {
				return viewsql.Relation{}, false
			}
			return viewsql.Relation{Quoted: t.QName(), Bare: t.Target}, true
		},
		View: func(owner, name string) (string, viewsql.Relation, string) {
			target := myName(name)
			reason := ""
			switch other, clash := taken[strings.ToLower(target)]; {
			case clash:
				reason = fmt.Sprintf("its name on the target would be %s, which is the name %s gets there", target, other)
			case len([]rune(target)) > maxNameLength:
				reason = fmt.Sprintf("its name is longer than the %d characters MySQL allows", maxNameLength)
			default:
				taken[strings.ToLower(target)] = "the view " + owner + "." + name
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
	return viewsql.Checker{From: viewsql.Oracle, To: viewsql.MySQL, Ident: myName,
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
