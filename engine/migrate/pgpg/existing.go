package pgpg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/migrate"
)

// Claim is one name a migration is about to take on a PostgreSQL target.
type Claim struct {
	// Kind is the kind of object to be created: table, sequence, index,
	// view, materialized view, type, domain, function or trigger.
	Kind string
	// Name is how the object is shown to the operator.
	Name string
	// Ident is the object's schema-qualified name in SQL syntax, quoted
	// where it needs to be. A trigger is written schema.table.trigger.
	// Anything after the name, such as a function's argument list, is
	// ignored.
	Ident string
	// Args tells functions of one name apart: the types of the input
	// arguments, as ArgTypesSQL writes them.
	Args string
}

// Existing is a claim whose name the target already holds.
type Existing struct {
	Claim
	// Holder is the kind of object that holds the name, which is not
	// always the kind being created: a view stands in the way of a table.
	Holder string
	// Target is the holder's name as the target knows it.
	Target string
	// Drop is the statement that removes the holder.
	Drop string
}

// ArgTypesSQL is the expression that identifies a function among those of
// the same name, for a pg_proc row aliased p. Two functions collide when
// their input argument types are the same, whatever the arguments are
// called, so the types are written out by schema and name: unlike
// format_type, that reads the same on both servers and under any
// search_path.
const ArgTypesSQL = `coalesce((SELECT pg_catalog.string_agg(tn.nspname || '.' || ty.typname, ',' ORDER BY a.ord)
		FROM pg_catalog.unnest(p.proargtypes::oid[]) WITH ORDINALITY AS a(oid, ord)
		JOIN pg_catalog.pg_type ty ON ty.oid = a.oid
		JOIN pg_catalog.pg_namespace tn ON tn.oid = ty.typnamespace), '')`

// relKindSQL names the kind of a pg_class row aliased c. Its upper-case
// form is also the word DROP takes.
const relKindSQL = `CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view'
		WHEN 'm' THEN 'materialized view' WHEN 'S' THEN 'sequence' WHEN 'i' THEN 'index' WHEN 'I' THEN 'index'
		WHEN 'f' THEN 'foreign table' ELSE 'type' END`

// relDropSQL writes the statement that removes a pg_class row aliased c in
// the schema aliased n. An index that enforces a constraint cannot be
// dropped by itself; the constraint is dropped instead.
const relDropSQL = `coalesce((SELECT format('ALTER TABLE %I.%I DROP CONSTRAINT IF EXISTS %I CASCADE', kn.nspname, kt.relname, k.conname)
			FROM pg_constraint k JOIN pg_class kt ON kt.oid = k.conrelid JOIN pg_namespace kn ON kn.oid = kt.relnamespace
			WHERE k.conindid = c.oid AND k.contype IN ('p', 'u', 'x') LIMIT 1),
		format('DROP %s IF EXISTS %I.%I CASCADE', upper(` + relKindSQL + `), n.nspname, c.relname))`

// existingSQL finds what already holds the claimed names. PostgreSQL keeps
// several namespaces per schema, and a name is taken when anything shares
// the namespace, not only an object of the same kind: tables, views,
// sequences and indexes are all relations; every relation but an index
// also owns a type of its name; functions, procedures and aggregates are
// told apart by their argument types; triggers are named per table.
//
// The array type PostgreSQL creates beside every type is left out, because
// it is renamed out of the way when its name is wanted.
const existingSQL = `WITH claim AS (
		SELECT w.ord, w.kind, w.args, i.id[1] AS a, i.id[2] AS b, i.id[3] AS c
		FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS w(kind, ident, args, ord),
			LATERAL (SELECT parse_ident(w.ident, false) AS id) i
	)
	SELECT w.ord, ` + relKindSQL + `, n.nspname || '.' || c.relname, ` + relDropSQL + `
	FROM claim w JOIN pg_namespace n ON n.nspname = w.a
	JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = w.b
	WHERE w.kind IN ('table', 'sequence', 'index', 'view', 'materialized view')
	UNION ALL
	SELECT w.ord, CASE WHEN c.oid IS NOT NULL THEN ` + relKindSQL + ` WHEN t.typtype = 'd' THEN 'domain' ELSE 'type' END,
		n.nspname || '.' || t.typname,
		CASE WHEN c.oid IS NOT NULL THEN ` + relDropSQL + `
			ELSE format('DROP %s IF EXISTS %I.%I CASCADE', CASE WHEN t.typtype = 'd' THEN 'DOMAIN' ELSE 'TYPE' END, n.nspname, t.typname) END
	FROM claim w JOIN pg_namespace n ON n.nspname = w.a
	JOIN pg_type t ON t.typnamespace = n.oid AND t.typname = w.b
	LEFT JOIN pg_class c ON c.oid = t.typrelid
	WHERE NOT EXISTS (SELECT 1 FROM pg_type e WHERE e.typarray = t.oid)
		AND (w.kind IN ('type', 'domain')
			OR (w.kind IN ('table', 'sequence', 'view', 'materialized view') AND t.typrelid = 0))
	UNION ALL
	SELECT w.ord, CASE p.prokind WHEN 'p' THEN 'procedure' WHEN 'a' THEN 'aggregate' ELSE 'function' END,
		format('%s.%s(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)),
		format('DROP ROUTINE IF EXISTS %I.%I(%s) CASCADE', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid))
	FROM claim w JOIN pg_namespace n ON n.nspname = w.a
	JOIN pg_proc p ON p.pronamespace = n.oid AND p.proname = w.b
	WHERE w.kind = 'function' AND w.args = ` + ArgTypesSQL + `
	UNION ALL
	SELECT w.ord, 'trigger', g.tgname || ' on ' || n.nspname || '.' || c.relname,
		format('DROP TRIGGER IF EXISTS %I ON %I.%I', g.tgname, n.nspname, c.relname)
	FROM claim w JOIN pg_namespace n ON n.nspname = w.a
	JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = w.b
	JOIN pg_trigger g ON g.tgrelid = c.oid AND g.tgname = w.c AND NOT g.tgisinternal
	WHERE w.kind = 'trigger'
	ORDER BY 1`

// FindExisting looks on the target for everything that already holds the
// name of an object the migration is about to create. It is shared by all
// pipelines that write to PostgreSQL.
//
// The DDL phases treat "already exists" as success, so that a job retried
// after an interruption passes over what it created the first time. Unless
// the names are looked up first, the same leniency lets an object that was
// on the target all along stand in for the source's, however different the
// two are.
func FindExisting(ctx context.Context, dst *pgx.Conn, claims []Claim) ([]Existing, error) {
	kinds, idents, args := make([]string, len(claims)), make([]string, len(claims)), make([]string, len(claims))
	for i, c := range claims {
		kinds[i], idents[i], args[i] = c.Kind, c.Ident, c.Args
	}
	rows, err := dst.Query(ctx, existingSQL, kinds, idents, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Existing
	seen := map[int]bool{}
	for rows.Next() {
		var ord int
		var e Existing
		if err := rows.Scan(&ord, &e.Holder, &e.Target, &e.Drop); err != nil {
			return nil, err
		}
		if seen[ord] {
			continue
		}
		seen[ord] = true
		e.Claim = claims[ord-1]
		out = append(out, e)
	}
	return out, rows.Err()
}

// isTable reports whether e is a table to be migrated that the target has
// as a table, which is the "Target is empty" check's business. Everything
// else belongs to "Existing objects".
func (e Existing) isTable() bool { return e.Kind == "table" && e.Holder == "table" }

// label is the claimed name, saying so when it is held by something of
// another kind; the operator would look for the wrong thing otherwise.
func (e Existing) label() string {
	if e.Holder == e.Kind {
		return e.Name
	}
	article := "a"
	if strings.HasPrefix(e.Holder, "i") || strings.HasPrefix(e.Holder, "a") {
		article = "an"
	}
	return e.Name + " (the name of " + article + " " + e.Holder + ")"
}

// ReportExisting records what FindExisting found: the list the operator is
// shown when asked what to do about it, and the two checks, which fail
// unless the job was told to keep or to replace what is there. place says
// where the tables were looked for, such as "on the target".
func ReportExisting(r *Report, found []Existing, policy, place string) {
	var tables []string
	var kinds []string
	others := map[string][]string{}
	listed := map[migrate.ExistingObject]bool{}
	for _, e := range found {
		// Two claims can be held by one object, which is listed once.
		if o := (migrate.ExistingObject{Kind: e.Holder, Name: e.Target}); !listed[o] {
			listed[o] = true
			r.Existing = append(r.Existing, o)
		}
		if e.isTable() {
			tables = append(tables, e.Name)
			continue
		}
		if _, ok := others[e.Kind]; !ok {
			kinds = append(kinds, e.Kind)
		}
		others[e.Kind] = append(others[e.Kind], e.label())
	}
	WriteExisting(r, policy, place, tables, kinds, others, "drop or rename them, or migrate into an empty database or schema")
}

// existingShown is how many names of one kind a failed check spells out.
const existingShown = 20

// WriteExisting records the two checks on what the target already holds:
// "Target is empty" for the tables to be migrated that are already there,
// and "Existing objects" for every other name that is taken, grouped by the
// kind of object the migration would create under it. Both fail unless the
// job was told to keep or to replace what is there, in which case they say
// exactly what that will do. The wording is the same for every target, so
// the pipelines that write to another database call this too.
func WriteExisting(r *Report, policy, place string, tables, kinds []string, others map[string][]string, advice string) {
	list := strings.Join(tables, ", ")
	switch {
	case len(tables) == 0:
		r.Add("Target is empty", "ok", "no conflicting tables "+place)
	case policy == migrate.ExistingSkip:
		r.Add("Target is empty", "warn", "these tables already exist "+place+" and are kept exactly as they are; they are left out of the migration: "+list)
	case policy == migrate.ExistingReplace:
		r.Add("Target is empty", "warn", "these tables will be DROPPED "+place+", with their rows and whatever depends on them there, and created again from the source: "+list)
	default:
		r.Add("Target is empty", "fail", "these tables already exist "+place+": "+list)
	}

	groups := make([]string, len(kinds))
	for i, kind := range kinds {
		names := others[kind]
		more := ""
		// What is about to be dropped is always named in full.
		if len(names) > existingShown && policy != migrate.ExistingReplace {
			more = fmt.Sprintf(" and %d more", len(names)-existingShown)
			names = names[:existingShown]
		}
		plural := kind + "s"
		if kind == "index" {
			plural = "indexes"
		}
		groups[i] = plural + ": " + strings.Join(names, ", ") + more
	}
	list = strings.Join(groups, "; ")
	switch {
	case len(kinds) == 0:
		r.Add("Existing objects", "ok", "nothing on the target has the name of an object to be migrated")
	case policy == migrate.ExistingSkip:
		r.Add("Existing objects", "warn", "the target already has these: "+list+
			". They are kept exactly as they are and left out of the migration; the source's are not created")
	case policy == migrate.ExistingReplace:
		r.Add("Existing objects", "warn", "these will be DROPPED on the target, with whatever depends on them there, and created again from the source: "+list)
	default:
		r.Add("Existing objects", "fail", "the target already has these: "+list+
			". The migration never replaces or merges into what the target already has: "+advice)
	}
}

// Decide turns what was found into what a job does about it under the
// policy chosen for it: the tables it leaves out and the other objects it
// keeps, or the statements that drop what it is to replace. A job decides
// once, before it creates anything, and remembers the answer in its state;
// afterwards its own objects exist too and can no longer be told apart.
func Decide(policy string, found []Existing) (skipped, kept, drops []string) {
	switch policy {
	case migrate.ExistingSkip:
		for _, e := range found {
			if e.Kind == "table" {
				skipped = append(skipped, e.Name)
			} else {
				kept = append(kept, KeptKey(e.Kind, e.Name))
			}
		}
	case migrate.ExistingReplace:
		seen := map[string]bool{}
		for _, e := range found {
			if !seen[e.Drop] {
				seen[e.Drop] = true
				drops = append(drops, e.Drop)
			}
		}
	}
	return skipped, kept, drops
}

// KeptKey is how a kept object is written in a job's state.
func KeptKey(kind, name string) string { return kind + " " + name }

// DropExisting removes what a job was told to replace. The statements are
// safe to run again, which a retry after an interrupted schema phase does.
func DropExisting(ctx context.Context, dst *pgx.Conn, drops []string, rep Reporter) error {
	if len(drops) == 0 {
		return nil
	}
	tables := 0
	for _, stmt := range drops {
		if _, err := dst.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %s", stmt, pgMessage(err))
		}
		if strings.HasPrefix(stmt, "DROP TABLE ") {
			tables++
		}
	}
	rep.Event("warn", fmt.Sprintf("dropped %d existing table(s) and %d other object(s) on the target, as chosen", tables, len(drops)-tables))
	return nil
}
