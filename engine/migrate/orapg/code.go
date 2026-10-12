package orapg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/storedcode"
)

// planCode has the stored code converted and decides what the job will
// create. The converter is told the tables as Oracle types their columns,
// and where the migration put them. A routine goes into the PostgreSQL
// schema of its Oracle schema, which is the converter's own default.
func (p *Plan) planCode(mode string) {
	opt := codesql.Options{Tables: oradef.Tables(p.Catalog)}
	for i := range opt.Tables {
		table := &opt.Tables[i]
		t := p.Table(table.Schema, table.Name)
		table.TargetSchema, table.Target = t.Schema, t.Target
		for j := range table.Columns {
			table.Columns[j].Target = t.Cols[j].Target
		}
	}
	storedcode.Plan(oracle.Kind, postgres.Kind, p.Code, opt, mode)
}

// finishCode creates the converted stored code and records what the job
// did with views and code. It runs last of all, so that no trigger fires
// for a row the migration itself wrote.
func (r *runner) finishCode(ctx context.Context) error {
	if len(storedcode.Planned(r.plan.Code)) > 0 {
		r.rep.Event("info", "creating the converted functions, procedures and triggers")
	}
	_, err := storedcode.Target{
		Exec: pgpg.CodeExec(func(ctx context.Context) (*pgx.Conn, error) {
			// A statement that ran out of time takes its connection with it.
			err := r.reconnect(ctx)
			return r.dst, err
		}),
		Previous: r.st.Code,
		Save: func(outcomes []storedcode.Outcome) {
			r.st.Code = outcomes
			r.save()
		},
		Event: r.rep.Event,
	}.Finish(ctx, r.plan.Code)
	if err != nil {
		return err
	}
	r.st.Objects = &storedcode.Summary{}
	r.st.Objects.Views(r.plan.Views, r.st.Views)
	r.st.Objects.Count(r.plan.Code, r.st.Code)
	r.save()
	return nil
}
