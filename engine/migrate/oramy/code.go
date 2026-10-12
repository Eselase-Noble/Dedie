package oramy

import (
	"context"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/storedcode"
)

// planCode has the stored code converted and decides what the job will
// create. The converter is told the tables as Oracle types their columns,
// the names the migration gave them, and the collation the tables are
// created with, which the text variables of a routine get as well.
func (p *Plan) planCode(mode string) {
	opt := codesql.Options{Tables: oradef.Tables(p.Catalog), Collation: p.target.collation()}
	for i := range opt.Tables {
		table := &opt.Tables[i]
		t := p.Table(table.Schema, table.Name)
		table.Target = t.Target
		for j := range table.Columns {
			table.Columns[j].Target = t.Cols[j].Target
		}
	}
	storedcode.Plan(oracle.Kind, mysql.Kind, p.Code, opt, mode)
}

// finishCode creates the converted stored code and records what the job
// did with views and code. It runs last of all, so that no trigger fires
// for a row the migration itself wrote.
func (r *runner) finishCode(ctx context.Context) error {
	if len(storedcode.Planned(r.plan.Code)) > 0 {
		r.rep.Event("info", "creating the converted functions, procedures and triggers")
	}
	_, err := storedcode.Target{
		Exec:     mymy.CodeExec(r.dst),
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
