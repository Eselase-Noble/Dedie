package mymy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Object is one name a migration is about to take in a MySQL target
// database.
type Object struct {
	// Kind is table, view, procedure, function, trigger, event or
	// foreign key.
	Kind string
	Name string
}

// Existing is an object whose name the target database already holds.
type Existing struct {
	Object
	// Holder is the kind of object that holds the name, which is not
	// always the kind being created: a view stands in the way of a table.
	Holder string
	// Target is the holder's name as the target knows it.
	Target string
	// Drops are the statements that remove the holder. Those that only
	// detach it from other tables come first.
	Drops []string
}

// existingQueries find, per kind of object, what already holds a name in
// the target database. Each returns the name, what holds it and, for a
// foreign key, its table. Tables and views share one namespace; procedures
// and functions have one each; triggers, events and foreign keys are named
// per database, not per table.
var existingQueries = map[string]string{
	"table": `SELECT table_name, IF(table_type = 'VIEW', 'view', 'table'), '' FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name IN (%s) ORDER BY 1`,
	"procedure": `SELECT routine_name, 'procedure', '' FROM information_schema.routines
		WHERE routine_schema = DATABASE() AND routine_type = 'PROCEDURE' AND routine_name IN (%s) ORDER BY 1`,
	"function": `SELECT routine_name, 'function', '' FROM information_schema.routines
		WHERE routine_schema = DATABASE() AND routine_type = 'FUNCTION' AND routine_name IN (%s) ORDER BY 1`,
	"trigger": `SELECT trigger_name, 'trigger', '' FROM information_schema.triggers
		WHERE trigger_schema = DATABASE() AND trigger_name IN (%s) ORDER BY 1`,
	"event": `SELECT event_name, 'event', '' FROM information_schema.events
		WHERE event_schema = DATABASE() AND event_name IN (%s) ORDER BY 1`,
	"foreign key": `SELECT constraint_name, 'foreign key', table_name FROM information_schema.table_constraints
		WHERE constraint_schema = DATABASE() AND constraint_type = 'FOREIGN KEY' AND constraint_name IN (%s) ORDER BY 1`,
}

// existingChunk is how many names one lookup asks for.
const existingChunk = 1000

// FindExisting looks in the target database for everything that already
// holds the name of an object the migration is about to create. It is
// shared by all pipelines that write to MySQL.
//
// The DDL phases treat "already exists" as success, so that a job retried
// after an interruption passes over what it created the first time. Unless
// the names are looked up first, the same leniency lets an object that was
// in the target all along stand in for the source's, however different the
// two are.
func FindExisting(ctx context.Context, dst Querier, objects []Object) ([]Existing, error) {
	var kinds []string
	names := map[string][]any{}
	for _, o := range objects {
		if _, ok := names[o.Kind]; !ok {
			kinds = append(kinds, o.Kind)
		}
		names[o.Kind] = append(names[o.Kind], o.Name)
	}
	var out []Existing
	for _, kind := range kinds {
		lookup := kind
		if kind == "view" {
			lookup = "table"
		}
		query, ok := existingQueries[lookup]
		if !ok {
			return nil, fmt.Errorf("no way to look for an existing %s on the target", kind)
		}
		for list := names[kind]; len(list) > 0; {
			n := min(len(list), existingChunk)
			rows, err := QueryStrings(ctx, dst, fmt.Sprintf(query, strings.TrimSuffix(strings.Repeat("?,", n), ",")), list[:n]...)
			if err != nil {
				return nil, err
			}
			list = list[n:]
			for _, row := range rows {
				e := Existing{Object: Object{Kind: kind, Name: row[0]}, Holder: row[1], Target: row[0]}
				switch e.Holder {
				case "table":
					if e.Drops, err = dropTable(ctx, dst, e.Target); err != nil {
						return nil, err
					}
				case "foreign key":
					e.Target = row[0] + " on " + row[2]
					e.Drops = []string{"ALTER TABLE " + mysql.Quote(row[2]) + " DROP FOREIGN KEY " + mysql.Quote(row[0])}
				default:
					e.Drops = []string{"DROP " + strings.ToUpper(e.Holder) + " IF EXISTS " + mysql.Quote(e.Target)}
				}
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// dropTable writes the statements that remove a table. MySQL refuses to
// drop a table other tables refer to and has no CASCADE to say otherwise,
// so their foreign keys are dropped first, which is what CASCADE would do.
func dropTable(ctx context.Context, dst Querier, table string) ([]string, error) {
	rows, err := QueryStrings(ctx, dst, `SELECT table_name, constraint_name FROM information_schema.referential_constraints
		WHERE constraint_schema = DATABASE() AND referenced_table_name = ? AND table_name <> ? ORDER BY 1, 2`, table, table)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		out = append(out, "ALTER TABLE "+mysql.Quote(row[0])+" DROP FOREIGN KEY "+mysql.Quote(row[1]))
	}
	return append(out, "DROP TABLE IF EXISTS "+mysql.Quote(table)), nil
}

// ReportExisting records what FindExisting found: the list the operator is
// shown when asked what to do about it, and the two checks, which fail
// unless the job was told to keep or to replace what is there.
func ReportExisting(r *migrate.Report, found []Existing, policy string) {
	var tables, kinds []string
	others := map[string][]string{}
	for _, e := range found {
		r.Existing = append(r.Existing, migrate.ExistingObject{Kind: e.Holder, Name: e.Target})
		if e.Kind == "table" && e.Holder == "table" {
			tables = append(tables, e.Name)
			continue
		}
		label := e.Target
		// Say so when the name is held by something of another kind; the
		// operator would look for the wrong thing otherwise.
		if e.Holder != e.Kind {
			label += " (the name of a " + e.Holder + ")"
		}
		if _, ok := others[e.Kind]; !ok {
			kinds = append(kinds, e.Kind)
		}
		others[e.Kind] = append(others[e.Kind], label)
	}
	// The wording of the two checks is the same for every kind of target.
	pgpg.WriteExisting(r, policy, "in the target database", tables, kinds, others, "drop or rename them, or migrate into an empty database")
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
				kept = append(kept, pgpg.KeptKey(e.Kind, e.Name))
			}
		}
	case migrate.ExistingReplace:
		// Every foreign key goes before any table does, so that the order
		// in which the tables are dropped does not matter.
		seen := map[string]bool{}
		for _, detach := range []bool{true, false} {
			for _, e := range found {
				for _, stmt := range e.Drops {
					if strings.Contains(stmt, " DROP FOREIGN KEY ") == detach && !seen[stmt] {
						seen[stmt] = true
						drops = append(drops, stmt)
					}
				}
			}
		}
	}
	return skipped, kept, drops
}

// DropExisting removes what a job was told to replace. A retry after an
// interrupted schema phase runs the statements again, by which time a
// foreign key may be gone already, alone or with its table.
func DropExisting(ctx context.Context, dst Querier, drops []string, rep migrate.Reporter) error {
	if len(drops) == 0 {
		return nil
	}
	tables := 0
	for _, stmt := range drops {
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			var myErr *driver.MySQLError
			gone := errors.As(err, &myErr) && (myErr.Number == 1091 || myErr.Number == 1146)
			if !gone || !strings.Contains(stmt, " DROP FOREIGN KEY ") {
				return fmt.Errorf("%s: %s", stmt, message(err))
			}
		}
		if strings.HasPrefix(stmt, "DROP TABLE ") {
			tables++
		}
	}
	rep.Event("warn", fmt.Sprintf("dropped %d existing table(s) and %d other object(s) on the target, as chosen", tables, len(drops)-tables))
	return nil
}
