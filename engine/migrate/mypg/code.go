package mypg

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/storedcode"
)

// showCreate returns the statement that creates a routine or trigger,
// exactly as MySQL shows it, which is the form the converter reads. It is
// empty when the account may not see the definition.
func showCreate(ctx context.Context, src mymy.Querier, kind, name string) string {
	rows, err := mymy.QueryStrings(ctx, src, "SHOW CREATE "+kind+" "+mysql.Quote(name))
	if err != nil || len(rows) == 0 || len(rows[0]) < 3 {
		return ""
	}
	return rows[0][2]
}

// loadCode reads the stored code in scope: the functions and procedures
// of the database, the triggers of the tables being migrated, and the
// scheduled events, which are only ever listed. A migration of
// hand-picked tables looks at their triggers and nothing else.
func (p *Plan) loadCode(ctx context.Context, src mymy.Querier, tableScope bool) error {
	// The source plan holds each definition without its DEFINER and the
	// database's name, which is how it is shown to the administrator.
	shown := map[string]string{}
	for _, list := range [][]mymy.Stmt{p.Plan.Post, p.Plan.Final} {
		for _, s := range list {
			shown[s.Kind+" "+s.Name] = s.SQL
		}
	}
	add := func(kind, name, table string) {
		o := &storedcode.Object{Kind: kind, Schema: p.Database, Name: name, Table: table, Label: name, Why: leftoverReasons[kind]}
		if kind != "event" {
			o.Source = showCreate(ctx, src, strings.ToUpper(kind), name)
		}
		if o.Definition = shown[kind+" "+name]; o.Definition == "" {
			o.Definition = o.Source
		}
		p.Code = append(p.Code, o)
	}
	if !tableScope {
		rows, err := mymy.QueryStrings(ctx, src, `SELECT routine_name, LOWER(routine_type) FROM information_schema.routines
			WHERE routine_schema = ? ORDER BY routine_type, routine_name`, p.Database)
		if err != nil {
			return err
		}
		for _, r := range rows {
			add(r[1], r[0], "")
		}
	}
	rows, err := mymy.QueryStrings(ctx, src, `SELECT trigger_name, event_object_table FROM information_schema.triggers
		WHERE trigger_schema = ? ORDER BY event_object_table, action_timing DESC, event_manipulation, action_order`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		// A trigger moves with its table.
		if p.Table(r[1]) != nil {
			add("trigger", r[0], r[1])
		}
	}
	for _, s := range p.Plan.Final {
		if s.Kind == "event" {
			add("event", s.Name, "")
		}
	}
	return nil
}

// exactText reports whether the source compares text exactly everywhere
// a routine could: in the columns of the migrated tables, and in the
// database's default, which the variables of a routine take. Unless it
// does, the converter marks every comparison of text for review, because
// PostgreSQL does not ignore case.
func exactText(ctx context.Context, src mymy.Querier, database string) bool {
	rows, err := mymy.QueryStrings(ctx, src, `SELECT
		(SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?
			AND default_collation_name NOT LIKE '%\_bin' AND default_collation_name NOT LIKE '%\_cs'),
		(SELECT COUNT(*) FROM information_schema.columns c JOIN information_schema.tables t
			ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
			WHERE c.table_schema = ? AND c.collation_name IS NOT NULL
			AND c.collation_name NOT LIKE '%\_bin' AND c.collation_name NOT LIKE '%\_cs')`, database, database)
	return err == nil && len(rows) == 1 && rows[0][0] == "0" && rows[0][1] == "0"
}

// planCode has the stored code converted and decides what the job will
// create.
func (p *Plan) planCode(ctx context.Context, src mymy.Querier, mode string) {
	opt := codesql.Options{TargetSchema: targetSchema}
	if mode != migrate.CodeOff && len(p.Code) > 0 {
		opt.ExactText = exactText(ctx, src, p.Database)
	}
	for _, t := range p.Tables {
		table := codesql.Table{Schema: p.Database, Name: t.Name, TargetSchema: targetSchema, Target: t.Name}
		for _, c := range t.Cols {
			table.Columns = append(table.Columns, codesql.Column{Name: c.Name, Type: c.ColumnType})
		}
		opt.Tables = append(opt.Tables, table)
	}
	storedcode.Plan(mysql.Kind, "postgres", p.Code, opt, mode)
}

// tableScopeNote says what a migration of hand-picked tables does with
// stored code.
const tableScopeNote = "A migration of hand-picked tables looks at the triggers of those tables only; functions and procedures are neither created nor listed."

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
