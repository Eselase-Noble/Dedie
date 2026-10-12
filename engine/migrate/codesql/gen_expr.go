package codesql

import (
	"strconv"
	"strings"
)

// value writes an expression that is used for its value. MySQL has no
// truth values of its own: a comparison there gives 1 or 0, and code
// stores it in a number. PostgreSQL keeps the two apart, so a comparison
// used as a value is converted to the number MySQL would have made.
func (g *gen) value(e expr) (string, typ) {
	s, t := g.expr(e)
	if g.from == MySQL && g.to == Postgres && t.class == cBool {
		if l, ok := e.(*litExpr); ok && l.kind == "bool" {
			if l.text == "TRUE" {
				return "1", tInt
			}
			return "0", tInt
		}
		if _, ok := e.(*parenExpr); ok {
			return s + "::int", tInt
		}
		return "(" + s + ")::int", tInt
	}
	return s, t
}

// cond writes an expression that is used as a condition. Coming from
// MySQL, a number stands for "not zero".
func (g *gen) cond(e expr) string {
	s, t := g.expr(e)
	if g.from == MySQL && g.to == Postgres && t.class != cBool {
		return wrap(s, e) + " <> 0"
	}
	return s
}

// arithmetic reports whether an expression passes a rounding applied to
// it on to its operands: rounding a sum or a product rounds away the far
// digits of the divisions inside it.
func arithmetic(e expr) bool {
	switch e := e.(type) {
	case *binExpr:
		return e.op == "+" || e.op == "-" || e.op == "*" || e.op == "/"
	case *unExpr:
		return e.op != "NOT"
	case *parenExpr:
		return len(e.items) == 1
	case *litExpr, *nameExpr:
		return true
	}
	return false
}

// expr writes any expression and says what kind of value it has.
func (g *gen) expr(e expr) (string, typ) {
	if !arithmetic(e) && g.rounded > 0 {
		defer func(n int) { g.rounded = n }(g.rounded)
		g.rounded = 0
	}
	switch e := e.(type) {
	case *litExpr:
		return g.literal(e)
	case *nameExpr:
		return g.name(e)
	case *callExpr:
		return g.call(e)
	case *binExpr:
		return g.binary(e)
	case *unExpr:
		if e.op == "NOT" {
			return "NOT " + wrapCond(g.cond(e.x), e.x), tBool
		}
		s, t := g.value(e.x)
		return e.op + wrap(s, e.x), t
	case *parenExpr:
		if len(e.items) == 1 {
			s, t := g.expr(e.items[0])
			return "(" + s + ")", t
		}
		var items []string
		for _, it := range e.items {
			s, _ := g.value(it)
			items = append(items, s)
		}
		return "(" + strings.Join(items, ", ") + ")", tUnknown
	case *caseExpr:
		return g.caseExpr(e)
	case *isExpr:
		if e.what == "NULL" {
			s, _ := g.value(e.x)
			if e.not {
				return wrap(s, e.x) + " IS NOT NULL", tBool
			}
			return wrap(s, e.x) + " IS NULL", tBool
		}
		s := wrapCond(g.cond(e.x), e.x)
		if g.from == MySQL {
			s = "(" + g.cond(e.x) + ")"
		}
		if e.not {
			return s + " IS NOT " + e.what, tBool
		}
		return s + " IS " + e.what, tBool
	case *betweenExpr:
		x, xt := g.value(e.x)
		lo, lt := g.value(e.lo)
		hi, _ := g.value(e.hi)
		g.textCompare(e.line, xt, lt, "BETWEEN")
		not := ""
		if e.not {
			not = "NOT "
		}
		return wrap(x, e.x) + " " + not + "BETWEEN " + wrap(lo, e.lo) + " AND " + wrap(hi, e.hi), tBool
	case *inExpr:
		x, xt := g.value(e.x)
		op := " IN ("
		if e.not {
			op = " NOT IN ("
		}
		if e.sub != nil {
			g.textCompare(e.line, xt, tUnknown, "IN")
			return wrap(x, e.x) + op + g.sql(e.sub) + ")", tBool
		}
		var items []string
		for _, it := range e.list {
			s, t := g.value(it)
			g.textCompare(e.line, xt, t, "IN")
			items = append(items, s)
		}
		return wrap(x, e.x) + op + strings.Join(items, ", ") + ")", tBool
	case *likeExpr:
		return g.like(e)
	case *existsExpr:
		return "EXISTS (" + g.sql(e.sub) + ")", tBool
	case *subExpr:
		t := tUnknown
		s := g.sqlTyped(e.sub, &t)
		return "(" + s + ")", t
	case *castExpr:
		return g.cast(e)
	case *attrExpr:
		return g.attr(e)
	case *extractExpr:
		return g.extract(e)
	case *intervalExpr:
		g.refuse(e.line, "an INTERVAL that is not added to or subtracted from a date")
		return "NULL", tUnknown
	case *starExpr:
		if len(e.qualifier) > 0 {
			return g.ident(e.qualifier[len(e.qualifier)-1]) + ".*", tUnknown
		}
		return "*", tUnknown
	case *rawExpr:
		g.refuse(e.line, "%s cannot be converted", e.what)
		return "NULL", tUnknown
	}
	panic("codesql: an expression of an unknown kind")
}

// wrapCond parenthesises a condition that is about to be negated or
// tested, unless it is a single term already.
func wrapCond(s string, e expr) string {
	switch e.(type) {
	case *nameExpr, *callExpr, *parenExpr, *existsExpr, *attrExpr:
		return s
	}
	return "(" + s + ")"
}

func (g *gen) literal(e *litExpr) (string, typ) {
	switch e.kind {
	case "number":
		if i := strings.IndexByte(e.text, '.'); i >= 0 && !strings.ContainsAny(e.text, "eE") {
			return e.text, typ{cNum, len(e.text) - i - 1}
		}
		if strings.ContainsAny(e.text, "eE") {
			if g.from == Oracle {
				return e.text, tNum
			}
			return e.text, tFloat
		}
		return e.text, tInt
	case "string":
		if e.text == "" && g.from == Oracle {
			// Oracle has no empty string: '' is NULL.
			return "NULL", tNull
		}
		return g.stringLit(e.text), tStr
	case "null":
		return "NULL", tNull
	case "bool":
		return e.text, tBool
	case "date":
		if g.from == Oracle {
			// An Oracle DATE has a time of day, here midnight.
			return "TIMESTAMP " + g.stringLit(e.text+" 00:00:00"), tTime
		}
		return "DATE " + g.stringLit(e.text), tDate
	case "timestamp":
		return "TIMESTAMP " + g.stringLit(e.text), tTime
	case "interval":
		if g.to == MySQL {
			g.refuse(e.line, "an interval that is not added to or subtracted from a date has no MySQL form")
			return "NULL", tInterval
		}
		s := "INTERVAL " + g.stringLit(e.text)
		if e.unit != "" {
			s += " " + e.unit
		}
		return s, tInterval
	}
	panic("codesql: a literal of an unknown kind")
}

// isLit reports whether an expression is a literal of the kind, and
// returns its text.
func isLit(e expr, kind string) (string, bool) {
	if l, ok := e.(*litExpr); ok && l.kind == kind {
		return l.text, true
	}
	return "", false
}

// intLit returns the value of a whole-number literal, with its sign.
func intLit(e expr) (int, bool) {
	neg := false
	if u, ok := e.(*unExpr); ok && u.op == "-" {
		neg, e = true, u.x
	}
	s, ok := isLit(e, "number")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// nonZeroLit reports whether an expression is a numeric literal other
// than zero, so dividing by it cannot fail.
func nonZeroLit(e expr) bool {
	if p, ok := e.(*parenExpr); ok && len(p.items) == 1 {
		return nonZeroLit(p.items[0])
	}
	s, ok := isLit(e, "number")
	if !ok {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f != 0
}

func (g *gen) caseExpr(e *caseExpr) (string, typ) {
	var b strings.Builder
	b.WriteString("CASE")
	if e.operand != nil {
		s, _ := g.value(e.operand)
		b.WriteString(" " + s)
	}
	result := tUnknown
	merge := func(t typ) {
		if !result.known() || result.class == cNull {
			result = t
		}
	}
	for _, w := range e.whens {
		var c string
		if e.operand != nil {
			c, _ = g.value(w.cond)
		} else {
			c = g.cond(w.cond)
		}
		r, t := g.value(w.result)
		merge(t)
		b.WriteString(" WHEN " + c + " THEN " + r)
	}
	if e.els != nil {
		r, t := g.value(e.els)
		merge(t)
		b.WriteString(" ELSE " + r)
	}
	b.WriteString(" END")
	return b.String(), result
}

// Names.

// name writes a reference: a variable, a column, a field of a record or
// of a trigger's row, or one of the words each language reads as a value.
func (g *gen) name(n *nameExpr) (string, typ) {
	first := strings.ToUpper(n.parts[0].name)
	if s, t, ok := g.rowRef(n, first); ok {
		return s, t
	}
	if strings.HasPrefix(first, "$") {
		i, _ := strconv.Atoi(first[1:])
		if i >= 1 && i <= len(g.r.params) {
			v := g.lookup(g.r.params[i-1].name)
			return v.out, v.typ
		}
		g.refuse(n.line, "%s names a parameter the function does not have", first)
		return "NULL", tUnknown
	}
	v := g.lookup(n.parts[0].name)
	inSQL := len(g.bound) > 0
	if v != nil && v.kind == "cursor" {
		v = nil
	}
	if len(n.parts) == 1 {
		col, table := g.findColumn(n.parts[0], nil)
		switch {
		case v != nil && col != nil && g.from == Oracle:
			// PL/SQL reads the name as the column.
			return g.columnOverVariable(n, col, table)
		case v != nil:
			if inSQL && col == nil && g.from == Oracle && g.to == MySQL && g.unknownTables() {
				g.note(n.line, "%s is a variable; if a table of this statement also has a column of that name, Oracle uses the column and MySQL the variable", n.parts[0].name)
			}
			return g.variable(v, n)
		case col != nil:
			return g.columnName(col), g.columnType(col)
		}
		if s, t, ok := g.special(n, first); ok {
			return s, t
		}
		if inSQL {
			return g.ident(n.parts[0]), tUnknown
		}
		if r := g.knownRoutine(n.parts); r != nil {
			// A function without arguments, called without parentheses.
			return g.objectName(n.parts) + "()", g.returnType(r)
		}
		g.note(n.line, "%s is not a variable or a column known here; it is written as it is", n.parts[0].name)
		return g.ident(n.parts[0]), tUnknown
	}
	// A qualified name: a field of a record, or a column of a table.
	if v != nil && (v.kind == "record" || v.kind == "row") && len(n.parts) == 2 {
		if _, table := g.findColumn(n.parts[1], &n.parts[0]); table == nil || g.from != Oracle {
			return g.field(v, n)
		}
	}
	if s, t, ok := g.special(n, first); ok {
		return s, t
	}
	if col, table := g.findColumn(n.parts[len(n.parts)-1], &n.parts[len(n.parts)-2]); table != nil {
		q := g.qualifier(table)
		if col != nil {
			return q + "." + g.columnName(col), g.columnType(col)
		}
		t := tUnknown
		if table.sub != nil {
			t = table.sub[strings.ToUpper(n.parts[len(n.parts)-1].name)]
		}
		return q + "." + g.ident(n.parts[len(n.parts)-1]), t
	}
	if v != nil && len(n.parts) == 2 {
		g.refuse(n.line, "%s.%s reads a part of %s, which is not a record", n.parts[0].name, n.parts[1].name, n.parts[0].name)
		return "NULL", tUnknown
	}
	if !inSQL {
		g.note(n.line, "%s is not a variable or a column known here; it is written as it is", describeExpr(n))
	}
	var parts []string
	for _, p := range n.parts {
		parts = append(parts, g.ident(p))
	}
	if g.to == MySQL && len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "."), tUnknown
}

func (g *gen) variable(v *variable, n *nameExpr) (string, typ) {
	switch v.kind {
	case "exception":
		g.refuse(n.line, "the exception %s is used as a value", v.name)
	case "record", "row":
		if g.to == MySQL {
			g.refuse(n.line, "the record %s is used as a whole, and MySQL has no record variables", v.name)
		}
	}
	return v.out, v.typ
}

// field writes record.field.
func (g *gen) field(v *variable, n *nameExpr) (string, typ) {
	f := v.fields[strings.ToUpper(n.parts[1].name)]
	if f == nil {
		if !v.anyField {
			g.refuse(n.line, "%s has no field %s", v.name, n.parts[1].name)
			return "NULL", tUnknown
		}
		if g.to == MySQL {
			g.refuse(n.line, "the type of %s.%s is not known, and MySQL needs a variable of a stated type for it", v.name, n.parts[1].name)
			return "NULL", tUnknown
		}
		return v.out + "." + g.ident(n.parts[1]), tUnknown
	}
	if g.to == MySQL {
		// The record was taken apart into one variable for each field.
		return f.out, f.typ
	}
	return v.out + "." + f.out, f.typ
}

// rowRef writes a reference to the row of a trigger: NEW.col, OLD.col.
func (g *gen) rowRef(n *nameExpr, first string) (string, typ, bool) {
	h := g.r.trigger
	if h == nil || (first != h.newName && first != h.oldName) {
		return "", tUnknown, false
	}
	if g.from == Oracle && !n.bind && !(g.inWhen && len(n.parts) == 2) {
		return "", tUnknown, false
	}
	row := "NEW"
	if first == h.oldName {
		row = "OLD"
	}
	if len(n.parts) != 2 {
		if g.to == MySQL || g.from == Oracle {
			g.refuse(n.line, "the row %s is used as a whole", first)
		}
		return row, typ{cRecord, -1}, true
	}
	if g.event != "" && ((row == "NEW" && g.event == "DELETE") || (row == "OLD" && g.event == "INSERT")) {
		// MySQL rejects the name outright where the other databases
		// give a row of NULLs.
		return "NULL", tNull, true
	}
	if t := g.findTable(h.table); t != nil {
		if c := t.column(g, n.parts[1]); c != nil {
			return row + "." + g.columnName(c), g.columnType(c), true
		}
	}
	return row + "." + g.ident(n.parts[1]), tUnknown, true
}

// findColumn looks for a column among the tables of the SQL statements
// being written. With a qualifier it also returns the table the
// qualifier names, even when the column is not known.
func (g *gen) findColumn(name ident, qualifier *ident) (*Column, *boundTable) {
	for i := len(g.bound) - 1; i >= 0; i-- {
		for _, b := range g.bound[i] {
			if qualifier != nil {
				match := false
				switch {
				case b.ref.alias != "":
					match = strings.EqualFold(b.ref.alias, qualifier.name)
				case len(b.ref.name) > 0:
					match = strings.EqualFold(b.ref.name[len(b.ref.name)-1].name, qualifier.name)
				}
				if !match {
					continue
				}
				if b.table != nil {
					return b.table.column(g, name), b
				}
				return nil, b
			}
			if b.table != nil {
				if c := b.table.column(g, name); c != nil {
					return c, b
				}
			}
		}
	}
	return nil, nil
}

// unknownTables reports whether a statement being written names a table
// the catalog does not describe, so that a name cannot be ruled out as
// one of its columns.
func (g *gen) unknownTables() bool {
	for _, level := range g.bound {
		for _, b := range level {
			if b.table == nil {
				return true
			}
		}
	}
	return false
}

// qualifier is how a table of the statement is referred to on the target.
func (g *gen) qualifier(b *boundTable) string {
	if b.ref.alias != "" {
		return g.quote(g.fold(ident{name: b.ref.alias}))
	}
	return g.tableName(b.ref.name)
}

// columnOverVariable writes a name that is both a variable and a column
// of the statement, coming from PL/SQL, where the column wins. PL/pgSQL
// is told to do the same for the whole routine; MySQL always takes the
// variable, so there the column is named through its table.
func (g *gen) columnOverVariable(n *nameExpr, col *Column, table *boundTable) (string, typ) {
	if g.to == MySQL {
		return g.qualifier(table) + "." + g.columnName(col), g.columnType(col)
	}
	return g.columnName(col), g.columnType(col)
}

// Operators.

func (g *gen) binary(e *binExpr) (string, typ) {
	switch e.op {
	case "AND", "OR":
		l, r := g.cond(e.l), g.cond(e.r)
		if b, ok := e.l.(*binExpr); ok && b.op != e.op && (b.op == "AND" || b.op == "OR") {
			l = "(" + l + ")"
		}
		if b, ok := e.r.(*binExpr); ok && (b.op == "AND" || b.op == "OR") {
			r = "(" + r + ")"
		}
		return l + " " + e.op + " " + r, tBool
	case "||":
		return g.concat(e)
	case "+", "-":
		return g.addition(e)
	case "*":
		return g.product(e)
	case "/", "DIV", "%":
		return g.division(e)
	case "^":
		l, _ := g.value(e.l)
		r, _ := g.value(e.r)
		if g.to == MySQL {
			return "POW(" + l + ", " + r + ")", tFloat
		}
		return "power(" + l + ", " + r + ")", tNum
	case "IS DISTINCT FROM", "IS NOT DISTINCT FROM":
		l, _ := g.value(e.l)
		r, _ := g.value(e.r)
		if g.to == MySQL {
			if e.op == "IS DISTINCT FROM" {
				return "NOT (" + wrap(l, e.l) + " <=> " + wrap(r, e.r) + ")", tBool
			}
			return wrap(l, e.l) + " <=> " + wrap(r, e.r), tBool
		}
		return wrap(l, e.l) + " " + e.op + " " + wrap(r, e.r), tBool
	}
	// A comparison.
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	g.textCompare(e.line, lt, rt, e.op)
	if g.from == Oracle && ((lt.temporal() && rt.class == cStr) || (lt.class == cStr && rt.temporal())) {
		g.note(e.line, "a date is compared with text; Oracle converts the text with the session's date format, so write the date as a date")
	}
	return wrapOperand(l, e.l) + " " + e.op + " " + wrapOperand(r, e.r), tBool
}

// wrapOperand parenthesises an operand of a comparison when it is a
// condition itself.
func wrapOperand(s string, e expr) string {
	switch e := e.(type) {
	case *binExpr:
		if _, cmp := comparisonOps[e.op]; cmp || e.op == "AND" || e.op == "OR" {
			return "(" + s + ")"
		}
	case *unExpr:
		if e.op == "NOT" {
			return "(" + s + ")"
		}
	case *isExpr, *betweenExpr, *inExpr, *likeExpr:
		return "(" + s + ")"
	}
	return s
}

// textCompare notes a comparison of text coming from MySQL, where the
// usual collations ignore case and PostgreSQL does not.
func (g *gen) textCompare(line int, l, r typ, op string) {
	if g.from != MySQL || g.opt.ExactText {
		return
	}
	if l.class == cStr || r.class == cStr {
		g.note(line, "text is compared (%s); MySQL's default collations ignore case and trailing spaces, PostgreSQL compares exactly", op)
	}
}

// flatten lists the operands of a chain of one operator.
func flatten(e expr, op string, out []expr) []expr {
	if b, ok := e.(*binExpr); ok && b.op == op {
		return append(flatten(b.l, op, out), b.r)
	}
	return append(out, e)
}

// concat writes a chain of "||".
func (g *gen) concat(e *binExpr) (string, typ) {
	ops := flatten(e, "||", nil)
	var parts []string
	solid, allLit := false, true
	for _, o := range ops {
		s, t := g.value(o)
		if text, ok := isLit(o, "string"); ok && text != "" {
			solid = true
		} else {
			allLit = false
		}
		if t.whole() && g.from == Oracle {
			// concat() and CONCAT_WS write a whole number as it is.
			parts = append(parts, s)
			continue
		}
		parts = append(parts, g.asText(o, s, t, "joined to text"))
	}
	switch {
	case g.from == Postgres:
		// NULL || x is NULL in PostgreSQL, as CONCAT(NULL, x) is in MySQL.
		return "CONCAT(" + strings.Join(parts, ", ") + ")", tStr
	case allLit && g.to == Postgres:
		return strings.Join(parts, " || "), tStr
	}
	// Oracle treats NULL as the empty string here, and an empty result
	// as NULL. concat() and CONCAT_WS skip NULLs; NULLIF restores the
	// second half unless a literal makes the result non-empty anyway.
	var s string
	if g.to == Postgres {
		s = "concat(" + strings.Join(parts, ", ") + ")"
	} else {
		s = "CONCAT_WS('', " + strings.Join(parts, ", ") + ")"
	}
	if !solid {
		s = "NULLIF(" + s + ", '')"
	}
	return s, tStr
}

// asText prepares a value that the source turns into text on its own.
// A whole number reads the same everywhere; other numbers, dates and
// truth values do not, and that is noted. use says what the text is
// for, to finish the note.
func (g *gen) asText(e expr, s string, t typ, use string) string {
	cast := func() string {
		if g.to == Postgres {
			return wrap(s, e) + "::text"
		}
		return "CAST(" + s + " AS CHAR)"
	}
	switch {
	case t.class == cStr || t.class == cNull:
		return s
	case t.whole():
		return cast()
	case t.class == cNum && g.from != Oracle:
		// MySQL and PostgreSQL both write a decimal with all the digits
		// of its scale.
		return cast()
	case t.class == cNum && g.to == Postgres:
		// Oracle writes a number without trailing zeros and without a
		// zero in front of the decimal point: 0.50 is ".5".
		return "regexp_replace(trim_scale(" + s + ")::text, '^(-?)0\\.', '\\1.')"
	case t.class == cNum || t.class == cFloat:
		if g.from == Oracle {
			g.note(e.exprLine(), "the number %s is %s; Oracle writes 0.5 as .5 and drops trailing zeros, %s does not", describeExpr(e), use, g.to)
		} else {
			g.note(e.exprLine(), "the number %s is %s; %s and %s write floating point numbers differently", describeExpr(e), use, g.from, g.to)
		}
		return cast()
	case t.class == cDate && g.from != Oracle:
		return cast()
	case t.class == cTime && t.scale == 0 && g.from == MySQL:
		// Without a fraction of a second both write the same text.
		return cast()
	case t.temporal():
		if g.from == Oracle {
			g.note(e.exprLine(), "the date %s is %s; Oracle writes it in the session's date format (NLS_DATE_FORMAT), %s in its own", describeExpr(e), use, g.to)
		} else {
			g.note(e.exprLine(), "the timestamp %s is %s; %s and %s write fractions of a second and time zones differently", describeExpr(e), use, g.from, g.to)
		}
		return cast()
	case t.class == cBool:
		g.note(e.exprLine(), "the truth value %s is %s; %s and %s write it differently", describeExpr(e), use, g.from, g.to)
		return cast()
	}
	g.note(e.exprLine(), "%s is %s and its type is not known here; if it is not text or a whole number, check how %s writes it", describeExpr(e), use, g.to)
	if g.from == MySQL {
		return cast()
	}
	return s
}

// numericResult is the type of a sum or a product.
func numericResult(l, r typ, product bool) typ {
	switch {
	case l.class == cFloat || r.class == cFloat:
		return tFloat
	case l.class == cInt && r.class == cInt:
		return tInt
	case l.numeric() && r.numeric():
		if l.scale < 0 || r.scale < 0 {
			return tNum
		}
		if product {
			return typ{cNum, l.scale + r.scale}
		}
		return typ{cNum, max(l.scale, r.scale)}
	}
	return tUnknown
}

func (g *gen) product(e *binExpr) (string, typ) {
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	if lt.class == cInterval || rt.class == cInterval {
		if g.to == MySQL {
			g.refuse(e.line, "an interval is multiplied outside of a date calculation")
		}
		return wrap(l, e.l) + " * " + wrap(r, e.r), tInterval
	}
	return wrapTerm(l, e.l) + " * " + wrapTerm(r, e.r), numericResult(lt, rt, true)
}

// wrapTerm parenthesises an operand of * or / that is a sum.
func wrapTerm(s string, e expr) string {
	switch e := e.(type) {
	case *binExpr:
		if e.op == "*" || e.op == "/" {
			return s
		}
		return "(" + s + ")"
	case *unExpr, *caseExpr:
		return "(" + s + ")"
	}
	return s
}

// division writes "/", MySQL's DIV and the remainder.
func (g *gen) division(e *binExpr) (string, typ) {
	rounded := g.rounded > 0
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	ls, rs := wrapTerm(l, e.l), wrapTerm(r, e.r)
	if _, ok := e.r.(*binExpr); ok {
		rs = "(" + r + ")"
	}
	safe := nonZeroLit(e.r)
	digits := func() {
		if !rounded && lt.class != cFloat && rt.class != cFloat {
			g.note(e.line, "the division %s keeps a different number of digits in %s than in %s; round the result if its last digits matter", describeExpr(e), g.to, g.from)
		}
	}
	zero := func() {
		if !safe {
			if g.from == Postgres {
				g.note(e.line, "the division %s raises an error in PostgreSQL when the divisor is 0 and gives NULL in MySQL", describeExpr(e))
			} else {
				g.note(e.line, "the division %s raises an error in Oracle when the divisor is 0 and gives NULL in MySQL", describeExpr(e))
			}
		}
	}
	switch {
	case e.op == "%":
		switch {
		case g.from == MySQL && !safe:
			// The remainder of a division by zero is NULL in MySQL.
			return ls + " % NULLIF(" + r + ", 0)", numericResult(lt, rt, false)
		case g.from == Postgres:
			zero()
		}
		return ls + " % " + rs, numericResult(lt, rt, false)
	case g.from == Oracle && g.to == Postgres:
		// Oracle divides numbers; PostgreSQL would drop the fraction when
		// both sides are integers.
		if lt.class != cNum && lt.class != cFloat && rt.class != cNum && rt.class != cFloat {
			rs = wrap(r, e.r) + "::numeric"
		}
		digits()
		if lt.class == cFloat || rt.class == cFloat {
			return ls + " / " + rs, tFloat
		}
		return ls + " / " + rs, tNum
	case g.from == Oracle:
		zero()
		digits()
		return ls + " / " + rs, tNum
	case g.from == MySQL && e.op == "DIV":
		if !safe {
			rs = "NULLIF(" + r + ", 0)"
		}
		if lt.class == cInt && rt.class == cInt {
			return ls + " / " + rs, tInt
		}
		return "trunc(" + wrap(l, e.l) + "::numeric / " + rs + ")", tWhole
	case g.from == MySQL:
		if !safe {
			rs = "NULLIF(" + r + ", 0)"
		}
		if lt.class != cNum && lt.class != cFloat && rt.class != cNum && rt.class != cFloat {
			ls = wrap(l, e.l) + "::numeric"
		}
		digits()
		if lt.class == cFloat || rt.class == cFloat {
			return ls + " / " + rs, tFloat
		}
		return ls + " / " + rs, tNum
	}
	// From PostgreSQL to MySQL.
	zero()
	switch {
	case lt.class == cInt && rt.class == cInt:
		return ls + " DIV " + rs, tInt
	case !lt.known() || !rt.known():
		g.note(e.line, "the types in the division %s are not known here; PostgreSQL drops the fraction when both sides are integers, MySQL never does", describeExpr(e))
	}
	digits()
	if lt.class == cFloat || rt.class == cFloat {
		return ls + " / " + rs, tFloat
	}
	return ls + " / " + rs, tNum
}

// like writes a LIKE test.
func (g *gen) like(e *likeExpr) (string, typ) {
	x, xt := g.value(e.x)
	p, pt := g.value(e.pattern)
	if e.insensitive {
		g.refuse(e.line, "ILIKE has no counterpart that ignores case in the same way")
	}
	g.textCompare(e.line, xt, pt, "LIKE")
	if g.from == MySQL && !g.opt.ExactText && xt.class != cStr && pt.class != cStr {
		g.note(e.line, "text is compared (LIKE); MySQL's default collations ignore case, PostgreSQL compares exactly")
	}
	op := " LIKE "
	if e.not {
		op = " NOT LIKE "
	}
	s := wrap(x, e.x) + op + wrap(p, e.pattern)
	switch {
	case e.escape != nil:
		esc, _ := g.value(e.escape)
		s += " ESCAPE " + esc
	case g.from == Oracle:
		// Oracle has no escape character unless one is named; the two
		// targets take the backslash for one.
		if text, ok := isLit(e.pattern, "string"); !ok || strings.Contains(text, `\`) {
			if g.to == MySQL {
				g.refuse(e.line, "a LIKE pattern that may contain a backslash: MySQL always reads it as an escape character")
			}
			s += " ESCAPE ''"
		}
	}
	return s, tBool
}

// attr writes a cursor attribute.
func (g *gen) attr(e *attrExpr) (string, typ) {
	if e.cursor == "SQL" {
		g.usesRowCount = true
		switch e.attr {
		case "ROWCOUNT":
			return rowCountVar, tInt
		case "FOUND":
			return "(" + rowCountVar + " > 0)", tBool
		case "NOTFOUND":
			return "(" + rowCountVar + " = 0)", tBool
		}
		g.refuse(e.line, "SQL%%%s", e.attr)
		return "NULL", tUnknown
	}
	if e.attr != "FOUND" && e.attr != "NOTFOUND" {
		g.refuse(e.line, "the cursor attribute %%%s", e.attr)
		return "NULL", tUnknown
	}
	if g.lastFetch != e.cursor && !(g.to == MySQL && g.cursors == 1) {
		c := strings.ToLower(e.cursor)
		g.note(e.line, "%s%%%s is read somewhere other than right after a FETCH from %s; the converted code tests the most recent fetch from any cursor", c, e.attr, c)
	}
	if g.to == MySQL {
		g.usesFound = true
		if e.attr == "FOUND" {
			return "(" + foundVar + " = 1)", tBool
		}
		return "(" + foundVar + " = 0)", tBool
	}
	if e.attr == "FOUND" {
		return "FOUND", tBool
	}
	return "NOT FOUND", tBool
}

// The variables the emitters add to a routine. The names are unlikely
// ones on purpose; a routine that declares one itself is refused.
const (
	rowCountVar = "dedie_rowcount"
	foundVar    = "dedie_found"
	messageVar  = "dedie_message"
	errTextVar  = "dedie_errtext"
	errStateVar = "dedie_errstate"
)

// extract writes EXTRACT(field FROM x).
func (g *gen) extract(e *extractExpr) (string, typ) {
	x, xt := g.value(e.x)
	switch {
	case g.to == Postgres:
		switch e.field {
		case "YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "QUARTER":
			if g.from == MySQL {
				return "EXTRACT(" + e.field + " FROM " + x + ")::int", tInt
			}
			return "EXTRACT(" + e.field + " FROM " + x + ")", tWhole
		case "SECOND":
			if g.from == MySQL {
				return "trunc(EXTRACT(SECOND FROM " + x + "))::int", tInt
			}
			return "EXTRACT(SECOND FROM " + x + ")", tNum
		}
	default:
		switch e.field {
		case "YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "QUARTER":
			return "EXTRACT(" + e.field + " FROM " + x + ")", tInt
		case "SECOND":
			if xt.class != cDate {
				g.note(e.line, "EXTRACT(SECOND ...) includes the fraction of a second in %s and not in MySQL", g.from)
			}
			return "EXTRACT(SECOND FROM " + x + ")", tInt
		case "DOW":
			return "(DAYOFWEEK(" + x + ") - 1)", tInt
		case "ISODOW":
			return "(WEEKDAY(" + x + ") + 1)", tInt
		case "DOY":
			return "DAYOFYEAR(" + x + ")", tInt
		}
	}
	g.refuse(e.line, "EXTRACT(%s ...) has no translation", e.field)
	return "NULL", tUnknown
}

// cast writes a conversion to a type.
func (g *gen) cast(e *castExpr) (string, typ) {
	x, xt := g.value(e.x)
	if len(e.typ.ref) > 0 {
		g.refuse(e.line, "a cast to %s", e.typ)
		return "NULL", tUnknown
	}
	m, ok := mapType(g.from, g.to, e.typ)
	if !ok {
		g.refuse(e.line, "a cast to %s has no translation", e.typ)
		return "NULL", tUnknown
	}
	if g.to == Postgres {
		if g.from == MySQL && (xt.class == cStr || !xt.known()) && m.typ.numeric() {
			g.note(e.line, "text is cast to a number; MySQL reads the digits it starts with and ignores the rest, PostgreSQL raises an error unless all of it is a number")
		}
		if g.from == MySQL && m.typ.class == cStr {
			return g.asText(e.x, x, xt, "cast to text"), tStr
		}
		return "CAST(" + x + " AS " + m.text + ")", m.typ
	}
	// MySQL casts to a short list of types of its own.
	switch m.typ.class {
	case cInt:
		return "CAST(" + x + " AS SIGNED)", tInt
	case cNum:
		if g.from == Postgres && len(e.typ.args) == 0 {
			if xt.numeric() {
				// A cast that only makes a division exact, which every
				// division in MySQL is.
				return x, typ{cNum, max(xt.scale, 0)}
			}
			g.note(e.line, "a cast to numeric without a precision becomes DECIMAL(65,20), which rounds decimal places beyond 20")
		}
		return "CAST(" + x + " AS " + strings.ToUpper(strings.Fields(m.text)[0]) + ")", m.typ
	case cStr:
		return g.asText(e.x, x, xt, "cast to text"), tStr
	case cDate:
		return "CAST(" + x + " AS DATE)", tDate
	case cTime:
		return "CAST(" + x + " AS " + strings.ToUpper(m.text) + ")", tTime
	case cFloat:
		return "CAST(" + x + " AS DOUBLE)", tFloat
	}
	g.refuse(e.line, "a cast to %s has no MySQL form", e.typ)
	return "NULL", tUnknown
}
