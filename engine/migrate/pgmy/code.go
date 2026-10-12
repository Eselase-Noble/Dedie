package pgmy

import (
	"context"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/storedcode"
)

// tableCollation is the collation the tables are created with, which the
// text variables of a converted routine are declared with as well.
const tableCollation = "utf8mb4_bin"

// loadCode reads the stored code in scope: the functions and procedures
// of the schemas and the triggers of the tables being migrated, each as
// PostgreSQL's catalog functions print it. A trigger comes with the
// function it executes, which is where its code is; such a function is not
// listed by itself. A migration of hand-picked tables looks at their
// triggers and nothing else.
//
// What the source has besides and MySQL has no place for (materialized
// views, check and exclusion constraints) is carried along to be listed.
func (p *Plan) loadCode(ctx context.Context, src *pgx.Conn, tableScope bool) error {
	if !tableScope {
		kind, plain := "CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END", "p.prokind IN ('f', 'p')"
		if p.VersionNum < 110000 {
			kind, plain = "'function'", "NOT p.proisagg AND NOT p.proiswindow"
		}
		rows, err := src.Query(ctx, `SELECT `+kind+`, n.nspname, p.proname,
				format('%I.%I(%s)', n.nspname, p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid)), pg_catalog.pg_get_functiondef(p.oid)
			FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = ANY($1) AND `+plain+` AND p.prorettype <> 'pg_catalog.trigger'::pg_catalog.regtype
			  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend x
			                  WHERE x.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND x.objid = p.oid AND x.deptype = 'e')
			ORDER BY 1, 4`, p.Schemas)
		if err != nil {
			return err
		}
		for rows.Next() {
			o := &storedcode.Object{}
			if err := rows.Scan(&o.Kind, &o.Schema, &o.Name, &o.Label, &o.Source); err != nil {
				rows.Close()
				return err
			}
			o.Definition, o.Why = o.Source, leftoverReasons["function"]
			p.Code = append(p.Code, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	oids := make([]uint32, len(p.Tables))
	for i, t := range p.Tables {
		oids[i] = t.OID
	}
	// MySQL fires the triggers of a table in the order they were created,
	// PostgreSQL in the order of their names; so that is the order here.
	rows, err := src.Query(ctx, `SELECT n.nspname, t.tgname, c.relname, format('%I on %I.%I', t.tgname, n.nspname, c.relname),
			pg_catalog.pg_get_triggerdef(t.oid), pg_catalog.pg_get_functiondef(t.tgfoid), t.tgenabled::text = 'D'
		FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT t.tgisinternal AND t.tgrelid = ANY($1) ORDER BY n.nspname, c.relname, t.tgname`, oids)
	if err != nil {
		return err
	}
	for rows.Next() {
		o := &storedcode.Object{Kind: "trigger", Why: leftoverReasons["trigger"]}
		var trigger, function string
		if err := rows.Scan(&o.Schema, &o.Name, &o.Table, &o.Label, &trigger, &function, &o.Disabled); err != nil {
			rows.Close()
			return err
		}
		// The converter reads the trigger and then its function; a
		// script creates them the other way round.
		o.Source = trigger + ";\n" + function
		o.Definition = function + ";\n\n" + trigger
		p.Code = append(p.Code, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The source plan holds the other definitions, read for the pipeline
	// that copies them as they are.
	for _, list := range [][]pgpg.Stmt{p.Plan.Pre, p.Plan.Post, p.Plan.Final} {
		for _, s := range list {
			if s.Kind == "function" || s.Kind == "trigger" {
				continue
			}
			if why, ok := leftoverReasons[s.Kind]; ok {
				p.Code = append(p.Code, &storedcode.Object{Kind: s.Kind, Name: s.Name, Label: s.Name, Definition: s.SQL, Why: why})
			}
		}
	}
	return nil
}

// planCode has the stored code converted and decides what the job will
// create. The converter is told the tables as the source types their
// columns, and the names the migration gave them.
func (p *Plan) planCode(ctx context.Context, src *pgx.Conn, mode string) error {
	opt := codesql.Options{Collation: tableCollation}
	if mode != migrate.CodeOff && len(p.Code) > 0 {
		oids := make([]uint32, len(p.Tables))
		byOID := map[uint32]*codesql.Table{}
		opt.Tables = make([]codesql.Table, len(p.Tables))
		for i, t := range p.Tables {
			oids[i] = t.OID
			opt.Tables[i] = codesql.Table{Schema: t.Schema, Name: t.Name, Target: t.Target}
			byOID[t.OID] = &opt.Tables[i]
		}
		rows, err := src.Query(ctx, `SELECT a.attrelid, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
			FROM pg_catalog.pg_attribute a WHERE a.attrelid = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attrelid, a.attnum`, oids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var oid uint32
			var c codesql.Column
			if err := rows.Scan(&oid, &c.Name, &c.Type); err != nil {
				return err
			}
			byOID[oid].Columns = append(byOID[oid].Columns, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	storedcode.Plan(postgres.Kind, mysql.Kind, p.Code, opt, mode)
	return nil
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
