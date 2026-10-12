// Package oradef reads from an Oracle source the definitions that the
// pipelines into another kind of database do not copy as they are: its
// views, which they translate where they can, and its PL/SQL, which they
// have converted where that can be done and hand to the administrator
// where it cannot.
//
// It asks the data dictionary and nothing else: no DBMS_METADATA, which
// needs more privileges for somebody else's schema than a migration
// account has reason to hold.
package oradef

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// in writes the schemas as the list of an IN condition.
func in(owners []string) string {
	quoted := make([]string, len(owners))
	for i, o := range owners {
		quoted[i] = literal(o)
	}
	return "IN (" + strings.Join(quoted, ", ") + ")"
}

// query runs a dictionary query and returns its rows as text; NULL comes
// back as an empty string.
func query(ctx context.Context, src oracle.Queryer, text string) ([][]string, error) {
	rows, err := src.QueryContext(ctx, text)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, oracle.Friendly(err)
		}
		row := make([]string, len(cols))
		for i, v := range raw {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out, oracle.Friendly(rows.Err())
}

// Views reads the views of the given schemas: the query of each as it was
// typed when the view was created, which is how Oracle keeps it, and its
// columns.
func Views(ctx context.Context, src oracle.Queryer, owners []string) ([]*viewsql.View, error) {
	rows, err := query(ctx, src, `SELECT owner, view_name, text FROM all_views WHERE owner `+in(owners)+` ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	var views []*viewsql.View
	byName := map[string]*viewsql.View{}
	for _, r := range rows {
		v := &viewsql.View{Schema: r[0], Name: r[1], Definition: r[2]}
		views = append(views, v)
		byName[v.Source()] = v
	}
	if len(views) == 0 {
		return nil, nil
	}
	rows, err = query(ctx, src, `SELECT c.owner, c.table_name, c.column_name, c.data_type FROM all_tab_columns c
		WHERE c.owner `+in(owners)+` AND EXISTS (SELECT 1 FROM all_views v WHERE v.owner = c.owner AND v.view_name = c.table_name)
		ORDER BY c.owner, c.table_name, c.column_id`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if v := byName[r[0]+"."+r[1]]; v != nil {
			v.Columns = append(v.Columns, viewsql.Column{Name: r[2], Class: viewsql.OracleClass(r[3])})
		}
	}
	return views, nil
}

// Dates returns a test for the names of the date and time columns of the
// tables in a catalog, which a pipeline into MySQL hands to the view
// translator; see viewsql.Options.
func Dates(cat *orasrc.Catalog) func(column string) bool {
	dates := map[string]bool{}
	for _, t := range cat.Tables {
		for _, c := range t.Cols {
			if viewsql.OracleClass(c.DataType) == viewsql.Temporal {
				dates[strings.ToUpper(c.Name)] = true
			}
		}
	}
	return func(column string) bool { return dates[strings.ToUpper(column)] }
}

// viewSource writes the statement that creates a view on the source.
func viewSource(v *viewsql.View) string {
	if v.Definition == "" {
		return ""
	}
	cols := ""
	if len(v.Columns) > 0 {
		names := make([]string, len(v.Columns))
		for i, c := range v.Columns {
			names[i] = oracle.Quote(c.Name)
		}
		cols = " (" + strings.Join(names, ", ") + ")"
	}
	return "CREATE OR REPLACE VIEW " + oracle.QName(v.Schema, v.Name) + cols + " AS\n" + strings.TrimSpace(v.Definition)
}

// Why each kind of stored code stays behind when no conversion is tried.
// The keys are the types of ALL_SOURCE in lower case.
var reasons = map[string]string{
	"function":     "it is written in PL/SQL",
	"procedure":    "it is written in PL/SQL",
	"package":      "packages have no counterpart on the target and are not converted",
	"package body": "packages have no counterpart on the target and are not converted",
	"trigger":      "it is written in PL/SQL",
	"type":         "user-defined types are not migrated",
	"type body":    "user-defined types are not migrated",
}

// codeOrder is the order the kinds are listed in: what others depend on
// comes first.
var codeOrder = []string{"type", "type body", "function", "procedure", "package", "package body", "trigger"}

// Code reads the stored code of the schemas in a catalog: functions,
// procedures and triggers, which the pipelines have converted, and
// packages, types and materialized views, which they only list. A
// migration of hand-picked tables is given the triggers of those tables
// and nothing else: the rest of the schema is none of its business.
//
// The source of a program unit is its text in ALL_SOURCE, joined in line
// order, which is the form the converter reads. Its definition, for a
// script, is that text behind CREATE OR REPLACE, ending with a slash on a
// line of its own, as SQL*Plus wants it. The triggers of a table follow
// one another by name.
func Code(ctx context.Context, src oracle.Queryer, cat *orasrc.Catalog, tableScope bool) ([]*storedcode.Object, error) {
	owners := in(cat.Owners)
	var out []*storedcode.Object
	if !tableScope {
		rows, err := query(ctx, src, `SELECT owner, mview_name, query FROM all_mviews WHERE owner `+owners+` ORDER BY 1, 2`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, &storedcode.Object{Kind: "materialized view", Schema: r[0], Name: r[1], Label: r[0] + "." + r[1],
				Why:        "materialized views are not migrated; this needs a table and something to refresh it, or the target's own kind of materialized view",
				Definition: "CREATE MATERIALIZED VIEW " + oracle.QName(r[0], r[1]) + " AS\n" + strings.TrimSpace(r[2])})
		}
	}

	type trigger struct {
		table    string
		inScope  bool
		disabled bool
	}
	triggers := map[string]trigger{}
	rows, err := query(ctx, src, `SELECT owner, trigger_name, table_owner, table_name, status FROM all_triggers WHERE owner `+owners)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		triggers[r[0]+"."+r[1]] = trigger{table: r[3], inScope: cat.Table(r[2], r[3]) != nil, disabled: r[4] == "DISABLED"}
	}
	rows, err = query(ctx, src, `SELECT owner, type, name, text FROM all_source WHERE owner `+owners+`
		AND type IN ('FUNCTION', 'PROCEDURE', 'PACKAGE', 'PACKAGE BODY', 'TRIGGER', 'TYPE', 'TYPE BODY') ORDER BY owner, type, name, line`)
	if err != nil {
		return nil, err
	}
	type unit struct {
		kind, owner, name string
		text              strings.Builder
	}
	var units []*unit
	var last *unit
	for _, r := range rows {
		kind := strings.ToLower(r[1])
		if last == nil || last.kind != kind || last.owner != r[0] || last.name != r[2] {
			last = &unit{kind: kind, owner: r[0], name: r[2]}
			units = append(units, last)
		}
		last.text.WriteString(r[3])
	}
	for _, kind := range codeOrder {
		for _, u := range units {
			label := u.owner + "." + u.name
			t := triggers[label]
			// A trigger on a table that is not migrated has nothing to
			// be created on.
			if u.kind != kind || (kind == "trigger" && !t.inScope) || (tableScope && kind != "trigger") {
				continue
			}
			text := strings.TrimSpace(u.text.String())
			out = append(out, &storedcode.Object{Kind: kind, Schema: u.owner, Name: u.name, Table: t.table, Label: label, Disabled: t.disabled,
				Source: text, Definition: "CREATE OR REPLACE " + text, End: "/", Why: reasons[kind]})
		}
	}
	return out, nil
}

// Tables describes the tables of a catalog to the converter: each column
// with its type as Oracle writes it.
func Tables(cat *orasrc.Catalog) []codesql.Table {
	out := make([]codesql.Table, len(cat.Tables))
	for i, t := range cat.Tables {
		out[i] = codesql.Table{Schema: t.Owner, Name: t.Name}
		for _, c := range t.Cols {
			length := sql.NullInt64{Int64: c.CharLength, Valid: c.CharLength > 0}
			// The catalog does not carry the length of a RAW; the longest
			// one can be declared with stands in for it.
			out[i].Columns = append(out[i].Columns, codesql.Column{Name: c.Name, Type: oracle.TypeName(c.DataType, length, c.Precision, c.Scale, 2000)})
		}
	}
	return out
}

// Leftovers lists what a migration out of Oracle into another kind of
// database leaves behind, with the statement that defines each object:
// the views that are left out or were dropped again after verification
// (results is what became of those a job created; nil before it ran),
// and the stored code the job does not create or created with a caveat,
// with the converted draft where there is one (outcomes likewise). to is
// the target's connector kind.
func Leftovers(to string, views []*viewsql.View, results []viewsql.Result, code []*storedcode.Object, outcomes []storedcode.Outcome) []migrate.Leftover {
	dropped := map[string]string{}
	for _, res := range results {
		if res.Lost() {
			dropped[res.Name] = res.Detail
		}
	}
	var out []migrate.Leftover
	for _, v := range views {
		reason := v.Reason
		if detail, was := dropped[v.Source()]; was {
			reason = "it was translated, but not kept on the target: " + detail
		} else if v.SQL != "" {
			continue
		}
		l := migrate.Leftover{Kind: "view", Name: v.Source(), Reason: reason, Source: viewSource(v)}
		if l.Source == "" {
			l.Reason += "; the source account may not read its definition, so it is not shown here"
		}
		out = append(out, l)
	}
	return append(out, storedcode.Leftovers(to, code, outcomes)...)
}

// TableScopeNote says what a migration of hand-picked tables does with
// stored code.
const TableScopeNote = "A migration of hand-picked tables looks at the triggers of those tables only; functions, procedures and packages are neither created nor listed."

// Convertible returns the objects the converter is asked about, which is
// what a preflight counts as stored code.
func Convertible(objects []*storedcode.Object) []*storedcode.Object {
	var out []*storedcode.Object
	for _, o := range objects {
		if o.Kind == "function" || o.Kind == "procedure" || o.Kind == "trigger" {
			out = append(out, o)
		}
	}
	return out
}

// Describe is the sentence for a preflight's "Not migrated" finding that
// says where the definitions can be had.
const Describe = "Functions, procedures and triggers are created only where their conversion can be vouched for. " +
	"The definitions of what stays behind, with the converted drafts where there are any, can be had as one script"

// NoPlace is why a view is left out when no table of its schema is
// migrated. PostgreSQL gets a schema for each Oracle schema that has
// tables to put in it, and a view on its own does not make one.
func NoPlace(owner string) string {
	return fmt.Sprintf("none of the tables of its schema, %s, are migrated, so the schema does not exist on the target", owner)
}
