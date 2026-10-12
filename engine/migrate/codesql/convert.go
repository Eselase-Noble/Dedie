package codesql

import (
	"fmt"
	"sort"
	"strings"
)

// Object is one routine of the source database.
type Object struct {
	// Kind is "function", "procedure" or "trigger".
	Kind string
	// Schema is the schema the object lives in on the source: the owner
	// in Oracle, the database in MySQL. Names the routine writes without
	// a schema are looked up there.
	Schema string
	Name   string
	// Table is the table a trigger belongs to.
	Table string
	// Source is the definition as the source database gives it:
	//
	//   - Oracle: the text of ALL_SOURCE joined in line order, or what
	//     DBMS_METADATA.GET_DDL returns. A leading CREATE OR REPLACE is
	//     optional.
	//   - MySQL: the statement SHOW CREATE FUNCTION, PROCEDURE or TRIGGER
	//     returns.
	//   - PostgreSQL: pg_get_functiondef for a function or a procedure.
	//     For a trigger, pg_get_triggerdef, a semicolon, and then
	//     pg_get_functiondef of the function the trigger executes.
	Source string
}

// Column is a column of a source table.
type Column struct {
	Name string
	// Type is the column's type as the source writes it: NUMBER(10,2),
	// varchar(40), timestamp without time zone.
	Type string
	// Target is the column's name on the target, when the pipeline gave
	// it one that differs from the usual translation of Name.
	Target string
}

// Table is a table of the source and where the migration put it.
type Table struct {
	Schema, Name string
	// TargetSchema and Target are the table's place on the target. Left
	// empty, they are worked out the way the pipelines name tables.
	TargetSchema, Target string
	Columns              []Column
}

// Routine is a function or a procedure the routine being converted may
// call.
type Routine struct {
	Name string
	// Returns is the type a function returns, as the source writes it.
	// It is empty for a procedure. Knowing it lets the converter tell,
	// for instance, whether "f(x) + 1" adds a number or a day.
	Returns string
}

// Options is what the converter is told about the migration a routine is
// part of. All of it is optional. Without the tables, a routine that
// depends on the type of a column is marked for review rather than
// converted, because the converter then has to guess.
type Options struct {
	// Tables are the tables being migrated.
	Tables []Table
	// Routines are the functions and procedures migrated together with
	// this one. A call to anything else that is not a built-in the
	// converter knows is pointed out. Signature fills one in from a
	// routine's definition.
	Routines []Routine
	// TargetSchema is the PostgreSQL schema the routine is created in and
	// its unqualified names are looked up in. It defaults to the source
	// schema's name for Oracle, and to no schema at all for MySQL.
	TargetSchema string
	// Collation is the collation text variables are given on a MySQL
	// target, so that they compare byte by byte like the migrated
	// columns do. It defaults to utf8mb4_bin.
	Collation string
	// ExactText says a MySQL source compares text exactly: its tables
	// and the database use a binary or case-sensitive collation. Without
	// it every comparison of text is marked for review, because MySQL's
	// default collations ignore case and PostgreSQL does not.
	ExactText bool
}

// Result is the outcome for one object.
type Result struct {
	// Status is Converted, Review or Unconverted.
	Status string
	// Statements are what to run on the target, in this order. They are
	// empty for an unconverted object.
	Statements []string
	// Notes say, in plain words and naming the line of the source, what
	// to check before trusting a routine marked for review, or why a
	// routine was not converted.
	Notes []string
}

const (
	// Converted means every construct of the routine was recognised and
	// rewritten in a way that keeps its behaviour.
	Converted = "converted"
	// Review means the routine was rewritten, and at least one rewrite
	// can behave differently; the notes say where and how.
	Review = "review"
	// Unconverted means the routine uses something that has no rewrite.
	// Nothing is emitted for it.
	Unconverted = "unconverted"
)

func dialect(kind string) Dialect {
	switch strings.ToLower(kind) {
	case "oracle":
		return Oracle
	case "postgres", "postgresql":
		return Postgres
	case "mysql", "mariadb":
		return MySQL
	}
	return 0
}

// Convert translates one function, procedure or trigger from one
// database's procedural language to another's. from and to are connector
// kinds: "oracle", "mysql", "postgres". It never fails: an object it
// cannot translate comes back Unconverted with the reason.
func Convert(from, to string, obj Object, opt Options) (res Result) {
	// A routine the converter trips over must not take the migration
	// down with it: it is one more routine left for a person.
	defer func() {
		if p := recover(); p != nil {
			res = Result{Status: Unconverted, Notes: []string{fmt.Sprintf("%s: %v", internalError, p)}}
		}
	}()
	f, t := dialect(from), dialect(to)
	supported := (f == Oracle && (t == Postgres || t == MySQL)) || (f == MySQL && t == Postgres) || (f == Postgres && t == MySQL)
	if !supported {
		return Result{Status: Unconverted, Notes: []string{fmt.Sprintf("routines are not converted from %s to %s", from, to)}}
	}
	var r *routine
	var err error
	switch f {
	case Oracle:
		r, err = parseOracle(obj.Source)
	case MySQL:
		r, err = parseMySQL(obj.Source)
	case Postgres:
		r, err = parsePostgres(obj.Source)
	}
	if err != nil {
		return Result{Status: Unconverted, Notes: []string{explain(err)}}
	}
	g := newGen(f, t, obj, opt, r)
	var stmts []string
	if t == Postgres {
		stmts = g.emitPostgres()
	} else {
		stmts = g.emitMySQL()
	}
	if len(g.refusals) > 0 {
		return Result{Status: Unconverted, Notes: sortNotes(g.refusals)}
	}
	if len(g.notes) > 0 {
		return Result{Status: Review, Statements: stmts, Notes: sortNotes(g.notes)}
	}
	return Result{Status: Converted, Statements: stmts}
}

// Signature reads the name and the return type of a function or a
// procedure from its definition, for Options.Routines. It reports false
// when the definition cannot be read.
func Signature(from string, obj Object) (Routine, bool) {
	var r *routine
	var err error
	switch dialect(from) {
	case Oracle:
		r, err = parseOracle(obj.Source)
	case MySQL:
		r, err = parseMySQL(obj.Source)
	case Postgres:
		r, err = parsePostgres(obj.Source)
	default:
		return Routine{}, false
	}
	if err != nil || r.kind == "trigger" {
		return Routine{}, false
	}
	out := Routine{Name: r.name[len(r.name)-1].name}
	if r.returns != nil {
		out.Returns = r.returns.String()
	}
	return out, true
}

// internalError starts the note of a routine that made the converter
// itself fail.
const internalError = "the converter could not handle this routine"

// explain words a parser's error as a note.
func explain(err error) string {
	switch e := err.(type) {
	case *unsupportedError:
		return fmt.Sprintf("line %d: %s cannot be converted", e.line, e.what)
	case *parseError:
		return fmt.Sprintf("line %d: the text could not be read as a routine: %s", e.line, e.msg)
	case *lexError:
		return fmt.Sprintf("line %d: the text could not be read: %s", e.line, e.msg)
	}
	return err.Error()
}

// sortNotes puts notes in the order of the lines they name. Each starts
// with "line N:", or with no line at all for what concerns the whole
// routine, which comes first.
func sortNotes(notes []string) []string {
	line := func(s string) int {
		var n int
		if _, err := fmt.Sscanf(s, "line %d:", &n); err != nil {
			return 0
		}
		return n
	}
	sort.SliceStable(notes, func(i, j int) bool { return line(notes[i]) < line(notes[j]) })
	return notes
}
