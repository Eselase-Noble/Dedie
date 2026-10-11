package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// clash is an object on the target that has the name of one the plan
// would create.
type clash struct {
	// Kind is the kind of object the plan would create under the name.
	Kind         string
	Schema, Name string
	// Held is the kind of object that has the name now. It is usually
	// the same, but a table's name can be taken by a view, for example.
	Held string
}

func (c clash) label() string { return c.Schema + "." + c.Name }

// shown is the name as the operator reads it, which says so when the
// name is held by something of another kind; they would look for the
// wrong thing otherwise.
func (c clash) shown() string {
	if c.Held != c.Kind {
		return c.label() + " (the name of a " + c.Held + ")"
	}
	return c.label()
}

// sharedNames are the kinds of object that share one namespace in an
// Oracle schema: a name taken by any of them is taken for all. Indexes
// and triggers each have a namespace of their own.
var sharedNames = map[string]bool{"table": true, "view": true, "materialized view": true, "sequence": true, "synonym": true,
	"function": true, "procedure": true, "package": true, "type": true}

// findClashes looks on the target for everything that stands where the
// plan would create an object: tables, sequences, views, synonyms,
// stored code, types, triggers and named indexes.
func findClashes(ctx context.Context, dst *sql.DB, p *Plan) ([]clash, error) {
	var schemas []string
	seen := map[string]bool{}
	for _, o := range p.Catalog.Owners {
		if s := p.Schemas[o]; !seen[s] {
			seen[s] = true
			schemas = append(schemas, literal(s))
		}
	}
	rows, err := queryText(ctx, dst, `SELECT owner, object_name, LOWER(object_type) FROM all_objects
		WHERE owner IN (`+strings.Join(schemas, ", ")+`) AND object_type IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW', 'SEQUENCE', 'SYNONYM',
			'FUNCTION', 'PROCEDURE', 'PACKAGE', 'TYPE', 'TRIGGER', 'INDEX')
		ORDER BY DECODE(object_type, 'MATERIALIZED VIEW', 0, 1)`)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	// held gives the kind of object behind each name, per namespace. A
	// materialized view is listed as a table as well, and comes first.
	held := map[string]string{}
	for _, r := range rows {
		space := r[2]
		if sharedNames[space] {
			space = "shared"
		}
		key := space + " " + r[0] + "." + r[1]
		if r[1] == checkpointTable || held[key] != "" {
			continue
		}
		held[key] = r[2]
	}

	var out []clash
	claimed := map[string]bool{}
	for _, list := range [][]Stmt{p.Pre, p.Code, p.Post, p.Final} {
		for _, s := range list {
			id := s.Object + " " + s.Schema + "." + s.Name
			if s.Object == "" || claimed[id] {
				continue
			}
			claimed[id] = true
			space := s.Object
			if sharedNames[space] {
				space = "shared"
			}
			if h := held[space+" "+s.Schema+"."+s.Name]; h != "" {
				out = append(out, clash{Kind: s.Object, Schema: s.Schema, Name: s.Name, Held: h})
			}
		}
	}
	return out, nil
}

// clashShown is how many names of one kind a check spells out.
const clashShown = 20

func plural(kind string) string {
	if kind == "index" {
		return "indexes"
	}
	return kind + "s"
}

// listed writes clashes out grouped by kind.
func listed(clashes []clash) string {
	var kinds []string
	byKind := map[string][]string{}
	for _, c := range clashes {
		if _, ok := byKind[c.Kind]; !ok {
			kinds = append(kinds, c.Kind)
		}
		byKind[c.Kind] = append(byKind[c.Kind], c.shown())
	}
	groups := make([]string, len(kinds))
	for i, kind := range kinds {
		names := byKind[kind]
		more := ""
		if len(names) > clashShown {
			more = fmt.Sprintf(" and %d more", len(names)-clashShown)
			names = names[:clashShown]
		}
		groups[i] = plural(kind) + ": " + strings.Join(names, ", ") + more
	}
	return strings.Join(groups, "; ")
}

// neverUnasked ends the checks that stop a job over what the target holds.
const neverUnasked = ". The migration never replaces or merges into what the target already has unless it is told to: drop or rename these, migrate into an empty schema, or choose to keep or to replace what exists"

// checkExisting records what the target already holds under the names
// of objects to be migrated: in Report.Existing for the operator to
// choose a policy by, and as two checks worded for the policy in force,
// "Target is empty" for the tables and "Existing objects" for the rest.
func checkExisting(r *migrate.Report, clashes []clash, policy string) {
	var tables, others []clash
	for _, c := range clashes {
		r.Existing = append(r.Existing, migrate.ExistingObject{Kind: c.Kind, Name: c.label()})
		if c.Kind == "table" {
			tables = append(tables, c)
		} else {
			others = append(others, c)
		}
	}
	names := make([]string, len(tables))
	for i, c := range tables {
		names[i] = c.shown()
	}
	tableList := strings.Join(names, ", ")
	switch {
	case len(tables) == 0:
		r.Add("Target is empty", "ok", "no conflicting tables on the target")
	case policy == migrate.ExistingSkip:
		r.Add("Target is empty", "warn", "these tables already exist on the target and are kept exactly as they are. They are left out of the migration: no rows are copied into them, changes to them on the source are not followed, and nothing is created on them: "+tableList)
	case policy == migrate.ExistingReplace:
		r.Add("Target is empty", "warn", "these tables will be DROPPED on the target, with the rows they hold and whatever depends on them there, and created again from the source: "+tableList)
	default:
		r.Add("Target is empty", "fail", "these tables already exist on the target: "+tableList+neverUnasked)
	}
	switch {
	case len(others) == 0:
		r.Add("Existing objects", "ok", "nothing on the target has the name of an object to be migrated")
	case policy == migrate.ExistingSkip:
		r.Add("Existing objects", "warn", "these already exist on the target and are kept exactly as they are; the source's versions are left out, and a kept sequence keeps its position: "+listed(others))
	case policy == migrate.ExistingReplace:
		r.Add("Existing objects", "warn", "these will be DROPPED on the target and created again from the source: "+listed(others))
	default:
		r.Add("Existing objects", "fail", "the target already has these: "+listed(others)+neverUnasked)
	}
}

// keep records the clashing objects in the job's state as kept. From
// then on the plan is built without them.
func (st *State) keep(clashes []clash, p *Plan) (tables, others int) {
	for _, c := range clashes {
		st.SkippedObjects = append(st.SkippedObjects, c.Kind+" "+c.label())
		if c.Kind != "table" {
			others++
			continue
		}
		tables++
		st.SkippedTables = append(st.SkippedTables, c.label())
		for _, t := range p.Tables {
			if t.Schema == c.Schema && t.Name == c.Name {
				st.SkippedSources = append(st.SkippedSources, t.Label())
			}
		}
	}
	return tables, others
}

// dropStatement is the statement that removes an object of the given
// kind. A table goes with the constraints that refer to it and past the
// recycle bin, so that its name and those of its indexes are free again.
func dropStatement(kind, schema, name string) string {
	q := oracle.QName(schema, name)
	switch kind {
	case "table":
		return "DROP TABLE " + q + " CASCADE CONSTRAINTS PURGE"
	case "type":
		return "DROP TYPE " + q + " FORCE"
	}
	return "DROP " + strings.ToUpper(kind) + " " + q
}

// dropClashes removes from the target everything that stands where the
// plan creates an object, as the operator chose. Tables go first: their
// indexes and triggers go with them, and are then no longer there to
// drop, which is not an error.
func dropClashes(ctx context.Context, dst *sql.DB, clashes []clash) (tables, others int, err error) {
	for _, tablesTurn := range []bool{true, false} {
		for _, c := range clashes {
			if (c.Held == "table") != tablesTurn {
				continue
			}
			_, err := dst.ExecContext(ctx, dropStatement(c.Held, c.Schema, c.Name))
			switch {
			case err == nil && tablesTurn:
				tables++
			case err == nil:
				others++
			case !notThere(err):
				return tables, others, fmt.Errorf("drop the existing %s %s on the target: %w", c.Held, c.label(), oracle.Friendly(err))
			}
		}
	}
	return tables, others, nil
}
