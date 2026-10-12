// Package oradef reads from an Oracle source the definitions that the
// pipelines into another kind of database do not copy as they are: its
// views, which they translate where they can, and its PL/SQL, which they
// do not translate at all and hand to the administrator instead.
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
	"dedie/engine/migrate/orasrc"
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

// Why each kind of stored code stays behind. The keys are the types of
// ALL_SOURCE in lower case.
var reasons = map[string]string{
	"function":     "PL/SQL is not translated",
	"procedure":    "PL/SQL is not translated",
	"package":      "PL/SQL is not translated",
	"package body": "PL/SQL is not translated",
	"trigger":      "triggers are written in PL/SQL, which is not translated",
	"type":         "user-defined types are not migrated",
	"type body":    "user-defined types are not migrated",
}

// codeOrder is the order the kinds are listed in: what others depend on
// comes first.
var codeOrder = []string{"type", "type body", "function", "procedure", "package", "package body", "trigger"}

// Leftovers lists what a migration out of Oracle into another kind of
// database leaves behind, with the statement that defines each object:
// the views that are left out or were dropped again after verification
// (results is what became of those a job created; nil before it ran),
// the materialized views, and the PL/SQL. A migration of hand-picked
// tables is told of the triggers on those tables only: the rest of the
// schema is none of its business.
//
// A program unit is written as CREATE OR REPLACE followed by its source
// from ALL_SOURCE, and ends with a slash on a line of its own, as SQL*Plus
// wants it.
func Leftovers(ctx context.Context, src oracle.Queryer, cat *orasrc.Catalog, tableScope bool, views []*viewsql.View, results []viewsql.Result) ([]migrate.Leftover, error) {
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
	owners := in(cat.Owners)

	if !tableScope {
		rows, err := query(ctx, src, `SELECT owner, mview_name, query FROM all_mviews WHERE owner `+owners+` ORDER BY 1, 2`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, migrate.Leftover{Kind: "materialized view", Name: r[0] + "." + r[1],
				Reason: "materialized views are not migrated; this needs a table and something to refresh it, or the target's own kind of materialized view",
				Source: "CREATE MATERIALIZED VIEW " + oracle.QName(r[0], r[1]) + " AS\n" + strings.TrimSpace(r[2])})
		}
	}

	// A trigger belongs to its table. In a migration of hand-picked
	// tables only theirs are in scope.
	onTable := map[string]bool{}
	rows, err := query(ctx, src, `SELECT owner, trigger_name, table_owner, table_name FROM all_triggers WHERE owner `+owners)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		onTable[r[0]+"."+r[1]] = cat.Table(r[2], r[3]) != nil
	}
	rows, err = query(ctx, src, `SELECT owner, type, name, text FROM all_source WHERE owner `+owners+`
		AND type IN ('FUNCTION', 'PROCEDURE', 'PACKAGE', 'PACKAGE BODY', 'TRIGGER', 'TYPE', 'TYPE BODY') ORDER BY owner, type, name, line`)
	if err != nil {
		return nil, err
	}
	type unit struct {
		kind, name string
		text       strings.Builder
	}
	var units []*unit
	var last *unit
	for _, r := range rows {
		kind, name := strings.ToLower(r[1]), r[0]+"."+r[2]
		if last == nil || last.kind != kind || last.name != name {
			last = &unit{kind: kind, name: name}
			units = append(units, last)
		}
		last.text.WriteString(r[3])
	}
	for _, kind := range codeOrder {
		for _, u := range units {
			if u.kind != kind || (tableScope && !(kind == "trigger" && onTable[u.name])) {
				continue
			}
			out = append(out, migrate.Leftover{Kind: kind, Name: u.name, Reason: reasons[kind],
				Source: "CREATE OR REPLACE " + strings.TrimSpace(u.text.String()), End: "/"})
		}
	}
	return out, nil
}

// Describe is the sentence for a preflight's "Not migrated" finding that
// says where the definitions can be had.
const Describe = "The PL/SQL, the materialized views and the views that are left out can be had as one script to convert by hand"

// NoPlace is why a view is left out when no table of its schema is
// migrated. PostgreSQL gets a schema for each Oracle schema that has
// tables to put in it, and a view on its own does not make one.
func NoPlace(owner string) string {
	return fmt.Sprintf("none of the tables of its schema, %s, are migrated, so the schema does not exist on the target", owner)
}
