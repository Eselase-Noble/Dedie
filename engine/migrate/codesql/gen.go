package codesql

import (
	"fmt"
	"regexp"
	"strings"
)

// gen writes one routine in the target language. It walks the syntax
// tree once; on the way it collects the notes that make the result a
// matter for review and the refusals that make it unconverted. Emission
// goes on after a refusal so that all of them are found, and the text is
// thrown away at the end.
type gen struct {
	from, to Dialect
	obj      Object
	opt      Options
	r        *routine

	notes    []string
	refusals []string
	seen     map[string]bool

	scopes []map[string]*variable
	// bound are the tables of the SQL statements being written,
	// innermost last; names inside them may be columns.
	bound [][]*boundTable
	// rounded counts the enclosing constructs that round the value being
	// written, which makes the far digits of a division irrelevant.
	rounded int

	routines map[string]*Routine
	schema   string // the target schema names are qualified with, or ""

	// event is the one event a MySQL trigger is being written for.
	event string
	// What the routine turned out to need from the emitter.
	usesRowCount bool
	usesFound    bool
	// foundAfter and countAfter say which kinds of statement PL/pgSQL's
	// FOUND and the row count are read after; see readers. Only those
	// have to keep the variables that stand in for them up to date.
	foundAfter, countAfter map[string]bool
	usesMessage            bool
	usesErrText            bool
	usesErrState           bool
	labels                 int
	loops                  []*loopFrame
	// handlerDepth counts the error handlers being written; MySQL does
	// not let their code leave by a label from outside them.
	handlerDepth int
	// lastFetch is the cursor of the statement just before the one being
	// written, when that was a FETCH.
	lastFetch string
	extra     []string // declarations the emitter adds to the outermost block
	prologue  []string // statements the emitter puts before the routine's own
	// What the MySQL emitter learns while writing the body: that a
	// cursor is fetched from, that a statement leaves the outermost
	// block, and whether the handler being written belongs to it.
	hasFetch      bool
	needBodyLabel bool
	handlerTop    bool
	// inWhen is set while the WHEN clause of a trigger is written, where
	// Oracle names the rows without a colon.
	inWhen bool
	// cursors counts the cursors the routine declares.
	cursors int
	// notFound are the MySQL handlers for NOT FOUND in force, innermost
	// last.
	notFound []*notFoundHandler
	// excDepth counts the enclosing blocks that have an error handler.
	excDepth int
	// noDataCaught counts the enclosing blocks that handle the absence
	// of a row themselves.
	noDataCaught int
	// inLoop is the loop whose body is being written.
	inLoop *loopStmt
}

// notFoundHandler is a MySQL handler for NOT FOUND, which PL/pgSQL has
// to spell out after each statement that can find nothing.
type notFoundHandler struct {
	h *handler
	// label is the block an EXIT handler leaves.
	label string
}

// variable is a name the routine declares.
type variable struct {
	name string // as declared
	out  string // as the target code writes it
	kind string // "var", "param", "cursor", "record", "exception", "row"
	typ  typ
	dt   *dataType
	// fields are the columns of a record, by upper-case name.
	fields map[string]*field
	// anyField says the record's columns are not known.
	anyField bool
	cursor   *cursorDecl
	exc      *exceptionDecl
	sqlstate string // of an exception
}

type field struct {
	out string
	typ typ
	// decl is the type to declare the field's variable with on MySQL,
	// where a record is taken apart into one variable for each field.
	decl string
}

// boundTable is a table of an SQL statement, with what the catalog says
// about it when the catalog knows it.
type boundTable struct {
	ref   *tableRef
	table *Table
	// sub are the result columns when the "table" is a subquery.
	sub map[string]typ
}

// loopFrame is a loop being written, for the statements that leave it.
type loopFrame struct {
	label     string // as the source wrote it, upper case; empty when it has none
	out       string // the label on the target
	nextLabel string // MySQL: the block to leave in order to start the next round
	used      bool
	block     bool // a labelled block rather than a loop
	// depth is how many error handlers enclosed the loop when it began.
	depth int
}

func newGen(from, to Dialect, obj Object, opt Options, r *routine) *gen {
	g := &gen{from: from, to: to, obj: obj, opt: opt, r: r, seen: map[string]bool{}, routines: map[string]*Routine{}}
	for i := range opt.Routines {
		name := opt.Routines[i].Name
		g.routines[strings.ToUpper(name)] = &opt.Routines[i]
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			g.routines[strings.ToUpper(name[dot+1:])] = &opt.Routines[i]
		}
	}
	if to == Postgres {
		g.schema = opt.TargetSchema
		if g.schema == "" && from == Oracle && obj.Schema != "" {
			g.schema = foldOracle(ident{name: obj.Schema})
		}
	}
	if g.opt.Collation == "" {
		g.opt.Collation = "utf8mb4_bin"
	}
	return g
}

// note records something to check. Notes name the line of the source
// they are about; line 0 is for what concerns the whole routine.
func (g *gen) note(line int, format string, args ...any) {
	g.add(&g.notes, line, fmt.Sprintf(format, args...))
}

// refuse records a construct that cannot be carried over.
func (g *gen) refuse(line int, format string, args ...any) {
	g.add(&g.refusals, line, fmt.Sprintf(format, args...))
}

func (g *gen) add(to *[]string, line int, text string) {
	if line > 0 {
		text = fmt.Sprintf("line %d: %s", line, text)
	}
	if !g.seen[text] {
		g.seen[text] = true
		*to = append(*to, text)
	}
}

// Scopes.

func (g *gen) push() { g.scopes = append(g.scopes, map[string]*variable{}) }
func (g *gen) pop()  { g.scopes = g.scopes[:len(g.scopes)-1] }

func (g *gen) declare(v *variable) *variable {
	if v.out == "" {
		v.out = g.quote(strings.ToLower(v.name))
	}
	g.scopes[len(g.scopes)-1][strings.ToUpper(v.name)] = v
	return v
}

func (g *gen) lookup(name string) *variable {
	up := strings.ToUpper(name)
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if v, ok := g.scopes[i][up]; ok {
			return v
		}
	}
	return nil
}

// declared reports whether any scope open now has the name.
func (g *gen) declared(name string) bool { return g.lookup(name) != nil }

// Names.

var (
	plainLower = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)
	plainAny   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)
)

// quote writes a name for the target, in quotes only when it needs them.
func (g *gen) quote(name string) string {
	if g.to == Postgres {
		if plainLower.MatchString(name) && !pgReserved[strings.ToUpper(name)] {
			return name
		}
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	if plainAny.MatchString(name) && !myReserved[strings.ToUpper(name)] {
		return name
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// foldOracle converts an Oracle identifier to the name used on the
// target, as the pipelines do: a name Oracle stores in upper case was
// written without quotes and becomes lower case; a name with lower-case
// letters was quoted on purpose and is kept exactly.
func foldOracle(id ident) string {
	if !id.quoted || id.name == strings.ToUpper(id.name) {
		return strings.ToLower(id.name)
	}
	return id.name
}

// fold gives the target's spelling of a source identifier that is not a
// variable: a column, a table, a routine.
func (g *gen) fold(id ident) string {
	switch g.from {
	case Oracle:
		return foldOracle(id)
	case Postgres:
		if !id.quoted {
			return strings.ToLower(id.name)
		}
	}
	return id.name
}

func (g *gen) ident(id ident) string { return g.quote(g.fold(id)) }

// sameName compares a name written in the routine with a name from the
// catalog, the way the source database would.
func (g *gen) sameName(written ident, catalog string) bool {
	if written.quoted && g.from != MySQL {
		return written.name == catalog
	}
	return strings.EqualFold(written.name, catalog)
}

// findTable looks a table up in what the pipeline said about the source.
func (g *gen) findTable(name []ident) *Table {
	if len(name) == 0 || len(name) > 2 {
		return nil
	}
	var found *Table
	for i := range g.opt.Tables {
		t := &g.opt.Tables[i]
		if !g.sameName(name[len(name)-1], t.Name) {
			continue
		}
		if len(name) == 2 {
			if g.sameName(name[0], t.Schema) {
				return t
			}
			continue
		}
		if g.obj.Schema == "" || strings.EqualFold(t.Schema, g.obj.Schema) || t.Schema == "" {
			return t
		}
		found = t
	}
	return found
}

func (t *Table) column(g *gen, name ident) *Column {
	for i := range t.Columns {
		if g.sameName(name, t.Columns[i].Name) {
			return &t.Columns[i]
		}
	}
	return nil
}

// columnName is the name a catalog column has on the target.
func (g *gen) columnName(c *Column) string {
	if c.Target != "" {
		return g.quote(c.Target)
	}
	return g.ident(ident{name: c.Name, quoted: true})
}

// columnType is the class of a catalog column.
func (g *gen) columnType(c *Column) typ {
	dt, ok := parseTypeText(g.from, c.Type)
	if !ok {
		return tUnknown
	}
	m, ok := mapType(g.from, g.to, dt)
	if !ok {
		return tUnknown
	}
	return m.typ
}

// tableName writes a table's name on the target.
func (g *gen) tableName(name []ident) string {
	if t := g.findTable(name); t != nil {
		target := t.Target
		if target == "" {
			target = g.fold(ident{name: t.Name, quoted: true})
		}
		if g.to == MySQL {
			return g.quote(target)
		}
		schema := t.TargetSchema
		if schema == "" {
			schema = g.schema
			if g.from == Oracle && t.Schema != "" {
				schema = foldOracle(ident{name: t.Schema, quoted: true})
			}
		}
		if schema == "" {
			return g.quote(target)
		}
		return g.quote(schema) + "." + g.quote(target)
	}
	return g.objectName(name)
}

// objectName writes the name of something that lives in a schema and is
// not in the catalog: a routine, a sequence, a table the pipeline did not
// mention.
func (g *gen) objectName(name []ident) string {
	last := g.ident(name[len(name)-1])
	if g.to == MySQL {
		// MySQL has no schemas inside a database; everything migrated
		// shares one namespace.
		return last
	}
	if len(name) > 1 {
		if g.from == MySQL {
			// What MySQL calls a database is not a schema of the target.
			return g.qualify(last)
		}
		return g.ident(name[len(name)-2]) + "." + last
	}
	return g.qualify(last)
}

func (g *gen) qualify(quoted string) string {
	if g.schema == "" {
		return quoted
	}
	return g.quote(g.schema) + "." + quoted
}

// knownRoutine finds the routine a call goes to among those the pipeline
// said it migrates as well.
func (g *gen) knownRoutine(name []ident) *Routine {
	var parts []string
	for _, p := range name {
		parts = append(parts, strings.ToUpper(p.name))
	}
	if r := g.routines[strings.Join(parts, ".")]; r != nil {
		return r
	}
	return g.routines[parts[len(parts)-1]]
}

// returnType is the class of what a known function returns.
func (g *gen) returnType(r *Routine) typ {
	if r == nil || r.Returns == "" {
		return tUnknown
	}
	dt, ok := parseTypeText(g.from, r.Returns)
	if !ok {
		return tUnknown
	}
	m, ok := mapType(g.from, g.to, dt)
	if !ok {
		return tUnknown
	}
	return m.typ
}

// Types.

// mapped translates a declared type, refusing the routine when it has
// no translation. what says what the type is of, for the message.
func (g *gen) mapped(d *dataType, what string) mapped {
	if d == nil {
		return mapped{typ: tUnknown}
	}
	if len(d.ref) > 0 {
		return g.mappedRef(d, what)
	}
	m, ok := mapType(g.from, g.to, d)
	if !ok {
		g.refuse(d.line, "the type %s of %s has no translation", d, what)
		return mapped{text: "text", typ: tUnknown}
	}
	if m.note != "" {
		g.note(d.line, "%s: %s", what, m.note)
	}
	if g.to == MySQL && m.text8 {
		m.text += " CHARACTER SET utf8mb4 COLLATE " + g.opt.Collation
	}
	return m
}

// mappedRef translates column%TYPE and its relatives.
func (g *gen) mappedRef(d *dataType, what string) mapped {
	if d.rowtype {
		g.refuse(d.line, "%s is a whole row (%s), which is only supported as the record of a loop", what, d)
		return mapped{text: "record", typ: typ{cRecord, -1}}
	}
	// variable%TYPE copies another variable.
	if len(d.ref) == 1 {
		if v := g.lookup(d.ref[0].name); v != nil && v.dt != nil {
			return g.mapped(v.dt, what)
		}
		g.refuse(d.line, "%s takes its type from %s, which is not a variable declared before it", what, d.ref[0].name)
		return mapped{text: "text", typ: tUnknown}
	}
	table, col := d.ref[:len(d.ref)-1], d.ref[len(d.ref)-1]
	t := g.findTable(table)
	var c *Column
	if t != nil {
		c = t.column(g, col)
	}
	if c == nil {
		if g.to == Postgres {
			// PL/pgSQL has the same notation, so the declaration is
			// right even though the type is not known here.
			return mapped{text: g.tableName(table) + "." + g.ident(col) + "%TYPE", typ: tUnknown}
		}
		g.refuse(d.line, "%s takes its type from the column %s, and MySQL needs the type spelled out; give the converter the table's columns", what, d)
		return mapped{text: "text", typ: tUnknown}
	}
	if g.to == Postgres {
		return mapped{text: g.tableName(table) + "." + g.columnName(c) + "%TYPE", typ: g.columnType(c)}
	}
	dt, ok := parseTypeText(g.from, c.Type)
	if !ok {
		g.refuse(d.line, "the type %s of the column %s could not be read", c.Type, d)
		return mapped{text: "text", typ: tUnknown}
	}
	dt.line = d.line
	return g.mapped(dt, what)
}

// writer builds the text of a routine line by line.
type writer struct {
	b      strings.Builder
	indent int
}

func (w *writer) line(format string, args ...any) {
	w.b.WriteString(strings.Repeat("  ", w.indent))
	if len(args) == 0 {
		w.b.WriteString(format)
	} else {
		fmt.Fprintf(&w.b, format, args...)
	}
	w.b.WriteByte('\n')
}

// text writes a line that is not a format.
func (w *writer) text(s string) { w.line("%s", s) }

func (w *writer) in()  { w.indent++ }
func (w *writer) out() { w.indent-- }

func (w *writer) String() string { return strings.TrimRight(w.b.String(), "\n") }

// stringLit writes a string literal for the target.
func (g *gen) stringLit(s string) string {
	s = strings.ReplaceAll(s, "'", "''")
	if g.to == MySQL {
		// MySQL reads a backslash in a literal as the start of an escape.
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + s + "'"
}

// isAtom reports whether an expression's text can stand next to any
// operator without parentheses.
func isAtom(e expr) bool {
	switch e := e.(type) {
	case *litExpr:
		return e.kind == "number" || e.kind == "string" || e.kind == "null" || e.kind == "bool"
	case *nameExpr, *callExpr, *parenExpr, *subExpr, *attrExpr, *extractExpr:
		return true
	}
	return false
}

// wrap parenthesises the text of an expression unless it is an atom.
func wrap(s string, e expr) string {
	if isAtom(e) {
		return s
	}
	return "(" + s + ")"
}

// describeExpr names an expression briefly for a note.
func describeExpr(e expr) string {
	switch e := e.(type) {
	case *nameExpr:
		var parts []string
		for _, p := range e.parts {
			parts = append(parts, p.name)
		}
		s := strings.Join(parts, ".")
		if e.bind {
			s = ":" + s
		}
		return s
	case *callExpr:
		return e.name[len(e.name)-1].name + "(...)"
	case *litExpr:
		if e.kind == "string" {
			return "'" + e.text + "'"
		}
		if e.kind == "null" {
			return "NULL"
		}
		return e.text
	case *parenExpr:
		if len(e.items) == 1 {
			return "(" + describeExpr(e.items[0]) + ")"
		}
	case *binExpr:
		return describeExpr(e.l) + " " + e.op + " " + describeExpr(e.r)
	case *unExpr:
		return e.op + " " + describeExpr(e.x)
	}
	return "an expression"
}
