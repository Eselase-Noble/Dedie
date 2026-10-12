package viewsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// Class is what kind of value a column holds, as far as comparing two
// databases goes.
type Class int

const (
	// Other is a type whose values the comparison only counts.
	Other Class = iota
	// Numeric is an exact number, Float an approximate one.
	Numeric
	Float
	Bool
	Text
	Temporal
	Binary
	Array
)

func (c Class) String() string {
	switch c {
	case Numeric, Float:
		return "a number"
	case Bool:
		return "a boolean"
	case Text:
		return "text"
	case Temporal:
		return "a date or time"
	case Binary:
		return "binary data"
	case Array:
		return "an array"
	}
	return "of another type"
}

// countable reports whether values of the class can be added up: numbers,
// and booleans as 0 and 1, which is what they become in MySQL.
func (c Class) countable() bool { return c == Numeric || c == Float || c == Bool }

// Column is a column of a view.
type Column struct {
	Name  string
	Class Class
}

// MySQLClass classifies a MySQL column by its information_schema data_type.
func MySQLClass(dataType string) Class {
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "year", "bit":
		return Numeric
	case "float", "double", "real":
		return Float
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return Text
	case "date", "datetime", "timestamp", "time":
		return Temporal
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return Binary
	}
	return Other
}

// PostgresClass classifies a PostgreSQL column by its type's name and
// category in pg_type.
func PostgresClass(typname, category string) Class {
	switch typname {
	case "int2", "int4", "int8", "numeric":
		return Numeric
	case "float4", "float8":
		return Float
	case "bool":
		return Bool
	case "bytea":
		return Binary
	}
	switch category {
	case "S":
		return Text
	case "D":
		return Temporal
	case "A":
		return Array
	}
	return Other
}

// OracleClass classifies an Oracle column by its DATA_TYPE.
func OracleClass(dataType string) Class {
	t := strings.ToUpper(dataType)
	switch {
	case t == "NUMBER" || t == "FLOAT":
		return Numeric
	case t == "BINARY_FLOAT" || t == "BINARY_DOUBLE":
		return Float
	case t == "VARCHAR2" || t == "NVARCHAR2" || t == "CHAR" || t == "NCHAR" || t == "VARCHAR":
		return Text
	case t == "DATE" || strings.HasPrefix(t, "TIMESTAMP"):
		return Temporal
	case t == "RAW" || t == "BLOB" || t == "LONG RAW":
		return Binary
	case t == "BOOLEAN":
		return Bool
	}
	return Other
}

// SummarySQL writes the query that describes what a view returns, in one
// row of text: the number of rows and then, for each column, how many of
// its values are not NULL and a total. The total is the sum of a numeric
// column and the combined length of a text column, without trailing
// blanks, which the databases do not agree on; other columns have none.
//
// The same query, each in its own dialect, is run against the source's
// view and the translated one. Equal answers do not prove the two views
// equal, but the ways a translation goes wrong without an error (a
// division that drops the remainder, an operator that means something
// else, a NULL treated as an empty string) all show up in them.
func SummarySQL(d Dialect, relation string, cols []Column) string {
	var list []string
	text := func(expr string) string {
		switch d {
		case MySQL:
			return "CAST(" + expr + " AS CHAR)"
		case Postgres:
			return "(" + expr + ")::text"
		}
		// The session's own number format may use a decimal comma.
		return "TO_CHAR(" + expr + ", 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''')"
	}
	list = append(list, text("COUNT(*)"))
	for _, c := range cols {
		name := Quote(d, c.Name)
		list = append(list, text("COUNT(CASE WHEN "+name+" IS NOT NULL THEN 1 END)"))
		total := ""
		switch {
		case c.Class == Numeric:
			total = "SUM(" + name + ")"
		case c.Class == Float && d == Postgres:
			// The sum of a real is a real, with six digits.
			total = "SUM(" + name + "::float8)"
		case c.Class == Float && d == Oracle:
			total = "CAST(SUM(CAST(" + name + " AS BINARY_DOUBLE)) AS NUMBER)"
		case c.Class == Float:
			total = "SUM(" + name + ")"
		case c.Class == Bool && d == Postgres:
			total = "SUM(" + name + "::int)"
		case c.Class == Bool && d == Oracle:
			total = "SUM(CASE WHEN " + name + " THEN 1 ELSE 0 END)"
		case c.Class == Bool:
			total = "SUM(" + name + ")"
		case c.Class == Text && d == MySQL:
			total = "SUM(CHAR_LENGTH(RTRIM(" + name + ")))"
		case c.Class == Text && d == Postgres:
			total = "SUM(length(rtrim(" + name + "::text)))"
		case c.Class == Text:
			total = "SUM(LENGTH(RTRIM(" + name + ")))"
		}
		switch {
		case total != "":
			list = append(list, text(total))
		case d == Oracle:
			list = append(list, "CAST(NULL AS VARCHAR2(1))")
		case d == Postgres:
			list = append(list, "NULL::text")
		default:
			list = append(list, "NULL")
		}
	}
	return "SELECT " + strings.Join(list, ", ") + " FROM " + relation
}

// Tolerances for comparing totals, relative to the larger of the two.
// Exact numbers may differ in their last digits because the databases
// carry a different number of them through a division or an average;
// approximate ones because they add up in a different order.
var (
	exactTolerance  = big.NewRat(1, 1_000_000_000)
	approxTolerance = big.NewRat(1, 1_000_000)
)

// sameTotal compares two totals as decimal numbers. An absent total is
// zero: the sum of no values.
func sameTotal(a, b *string, tolerance *big.Rat) bool {
	parse := func(s *string) (*big.Rat, bool) {
		if s == nil || strings.TrimSpace(*s) == "" {
			return new(big.Rat), true
		}
		return new(big.Rat).SetString(strings.TrimSpace(*s))
	}
	x, okX := parse(a)
	y, okY := parse(b)
	if !okX || !okY {
		// Not a number either database prints alike, such as NaN.
		return a != nil && b != nil && strings.EqualFold(strings.TrimSpace(*a), strings.TrimSpace(*b))
	}
	diff := new(big.Rat).Sub(x, y)
	diff.Abs(diff)
	scale := big.NewRat(1, 1)
	for _, v := range []*big.Rat{x, y} {
		if abs := new(big.Rat).Abs(v); abs.Cmp(scale) > 0 {
			scale = abs
		}
	}
	return diff.Cmp(new(big.Rat).Mul(tolerance, scale)) <= 0
}

func shown(s *string) string {
	if s == nil {
		return "nothing"
	}
	return *s
}

// Compare holds the answers of SummarySQL for the source's view and the
// translated one against each other, and says how they differ; it returns
// an empty string when they do not. names are the source's column names
// as they should read on the target.
func Compare(names []string, src, dst []Column, a, b []*string, foldCase bool) string {
	if len(src) != len(dst) {
		return fmt.Sprintf("it has %d column(s) on the source and %d on the target", len(src), len(dst))
	}
	for i := range src {
		if names[i] != dst[i].Name && !(foldCase && strings.EqualFold(names[i], dst[i].Name)) {
			return fmt.Sprintf("column %d is called %s on the source and %s on the target", i+1, src[i].Name, dst[i].Name)
		}
		if x, y := src[i].Class, dst[i].Class; x != y && x != Other && y != Other && !(x.countable() && y.countable()) {
			return fmt.Sprintf("column %s is %s on the source and %s on the target", src[i].Name, x, y)
		}
	}
	if len(a) != 1+2*len(src) || len(b) != len(a) || a[0] == nil || b[0] == nil {
		return "the comparison did not return what was asked for"
	}
	if *a[0] != *b[0] {
		return fmt.Sprintf("it returns %s row(s) on the source and %s on the target", *a[0], *b[0])
	}
	for i := range src {
		name := src[i].Name
		if x, y := a[1+2*i], b[1+2*i]; shown(x) != shown(y) {
			return fmt.Sprintf("column %s has %s value(s) that are not NULL on the source and %s on the target", name, shown(x), shown(y))
		}
		x, y := a[2+2*i], b[2+2*i]
		switch sc, dc := src[i].Class, dst[i].Class; {
		case sc.countable() && dc.countable():
			tolerance := exactTolerance
			if sc == Float || dc == Float {
				tolerance = approxTolerance
			}
			if !sameTotal(x, y, tolerance) {
				return fmt.Sprintf("the values of column %s add up to %s on the source and %s on the target", name, shown(x), shown(y))
			}
		case sc == Text && dc == Text:
			if !sameTotal(x, y, new(big.Rat)) {
				return fmt.Sprintf("the text in column %s is %s character(s) long in all on the source and %s on the target", name, shown(x), shown(y))
			}
		}
	}
	return ""
}

// What became of a view the migration tried to create.
const (
	// Verified: created, and it returns the same as on the source.
	Verified = "verified"
	// Unverified: created and kept, but the comparison could not be made
	// in the time allowed.
	Unverified = "not verified"
	// Dropped: the target refused it or it returned something else; it
	// is not on the target.
	Dropped = "dropped"
)

// Result is what became of one view. A job keeps these in its state.
type Result struct {
	// Name is the view's name on the source, Target its name on the target.
	Name   string `json:"name"`
	Target string `json:"target"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DefaultTimeout is how long the comparison waits for either database to
// describe one view.
const DefaultTimeout = 60 * time.Second

// Checker creates translated views on a target and verifies them. A
// pipeline fills in how its two databases are reached.
type Checker struct {
	From, To Dialect
	// Ident maps a column name to the target, as in Naming.
	Ident func(name string) string
	// SourceRelations writes how the view is named in a query on the
	// source. More than one way may be given, to be tried in turn: the
	// view as of the moment the tables were copied, and failing that as
	// it is now.
	SourceRelations func(v *View) []string
	// SourceRow and TargetRow run a query that returns one row and give
	// its values as text, nil for NULL.
	SourceRow, TargetRow func(ctx context.Context, query string) ([]*string, error)
	// Create runs the view's statement on the target. A view that is
	// already there counts as created: a job that was interrupted here
	// finds its own views when it is run again.
	Create func(ctx context.Context, v *View) error
	Drop   func(ctx context.Context, v *View) error
	// TargetColumns reads the columns the created view has.
	TargetColumns func(ctx context.Context, v *View) ([]Column, error)
	// Timeout bounds each side of one comparison; zero means DefaultTimeout.
	Timeout time.Duration
	// Event reports to the operator.
	Event func(level, message string)
}

// Finish creates the views, in the order Plan put them, and verifies each
// against the source before going on to the next. It must be called when
// both databases hold the same rows: for a one-time copy right after the
// tables were validated, for a migration that follows changes at cutover.
//
// A view the target refuses, or that does not return the same as on the
// source, is dropped again and reported; it never fails the job. A view
// whose comparison takes longer than the timeout is kept and reported as
// not verified. The only error is the context's.
func (c Checker) Finish(ctx context.Context, views []*View) ([]Result, error) {
	gone := map[*View]bool{}
	var results []Result
	counts := map[string]int{}
	for _, v := range Kept(views) {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		r := Result{Name: v.Source(), Target: v.Label}
		r.Status, r.Detail = c.one(ctx, v, gone)
		if err := ctx.Err(); err != nil {
			return results, err
		}
		switch r.Status {
		case Verified:
			c.Event("info", fmt.Sprintf("created view %s; it returns the same as %s on the source", v.Label, v.Source()))
		case Unverified:
			c.Event("warn", fmt.Sprintf("created view %s, but it was not verified against the source: %s", v.Label, r.Detail))
		default:
			gone[v] = true
			c.Event("warn", fmt.Sprintf("view %s was not migrated: %s", v.Source(), r.Detail))
		}
		counts[r.Status]++
		results = append(results, r)
	}
	if len(results) > 0 {
		c.Event("info", fmt.Sprintf("views: %d created and verified, %d created but not verified, %d not kept", counts[Verified], counts[Unverified], counts[Dropped]))
	}
	return results, nil
}

// one creates and verifies a single view.
func (c Checker) one(ctx context.Context, v *View, gone map[*View]bool) (status, detail string) {
	for _, need := range v.needs {
		if gone[need] {
			return Dropped, fmt.Sprintf("it reads the view %s, which was not kept", need.Source())
		}
	}
	if err := c.Create(ctx, v); err != nil {
		return Dropped, "the target did not accept the translated definition: " + err.Error()
	}
	status, detail = c.verify(ctx, v)
	if status == Dropped {
		if err := c.Drop(context.WithoutCancel(ctx), v); err != nil {
			detail += fmt.Sprintf(". It could not be dropped from the target again (%s): drop %s there by hand", err, v.Label)
		}
	}
	return status, detail
}

func (c Checker) verify(ctx context.Context, v *View) (status, detail string) {
	cols, err := c.TargetColumns(ctx, v)
	if err != nil {
		return Dropped, "its columns could not be read on the target: " + err.Error()
	}
	names := make([]string, len(v.Columns))
	for i, col := range v.Columns {
		names[i] = c.Ident(col.Name)
	}
	// What can be told from the columns alone needs no query.
	if len(cols) == len(v.Columns) {
		empty := make([]*string, 1+2*len(cols))
		zero := "0"
		empty[0] = &zero
		if diff := Compare(names, v.Columns, cols, empty, empty, c.To == MySQL); diff != "" {
			return Dropped, diff
		}
	} else {
		return Dropped, Compare(names, v.Columns, cols, nil, nil, c.To == MySQL)
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	var a, b []*string
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, relation := range c.SourceRelations(v) {
			side, cancel := context.WithTimeout(ctx, timeout)
			a, errA = c.SourceRow(side, SummarySQL(c.From, relation, v.Columns))
			timedOut := errors.Is(side.Err(), context.DeadlineExceeded)
			cancel()
			if errA == nil {
				return
			}
			if timedOut {
				errA = context.DeadlineExceeded
				return
			}
		}
	}()
	side, cancel := context.WithTimeout(ctx, timeout)
	b, errB = c.TargetRow(side, SummarySQL(c.To, v.Target.Quoted, cols))
	if errB != nil && errors.Is(side.Err(), context.DeadlineExceeded) {
		errB = context.DeadlineExceeded
	}
	cancel()
	wg.Wait()

	switch {
	case ctx.Err() != nil:
		return Unverified, "the job was stopped"
	case errors.Is(errA, context.DeadlineExceeded):
		return Unverified, fmt.Sprintf("reading it on the source took longer than %s", timeout)
	case errors.Is(errB, context.DeadlineExceeded):
		return Unverified, fmt.Sprintf("reading it on the target took longer than %s", timeout)
	case errB != nil:
		return Dropped, "it was created, but reading it on the target fails: " + errB.Error()
	case errA != nil:
		return Dropped, "it could not be read on the source, so the translation could not be checked: " + errA.Error()
	}
	if diff := Compare(names, v.Columns, cols, a, b, c.To == MySQL); diff != "" {
		return Dropped, "it does not return the same as on the source: " + diff
	}
	return Verified, ""
}

// Querier is the part of *sql.DB and *sql.Conn that Row needs.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Row runs a query that returns one row and gives its values back as
// text, nil for NULL. It is what a Checker's SourceRow or TargetRow is for
// a database reached through database/sql.
func Row(ctx context.Context, db Querier, query string) ([]*string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	raw := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	out := make([]*string, len(raw))
	for i := range raw {
		if raw[i].Valid {
			out[i] = &raw[i].String
		}
	}
	return out, rows.Err()
}
