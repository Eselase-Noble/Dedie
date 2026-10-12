package codesql

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// emitPostgres writes the routine in PL/pgSQL and returns the statements
// that create it: one for a function or a procedure, two for a trigger,
// whose code PostgreSQL keeps in a function of its own.
func (g *gen) emitPostgres() []string {
	r := g.r
	g.prescan()
	g.push()
	defer g.pop()
	switch r.kind {
	case "trigger":
		return g.pgTrigger()
	}
	name := g.objectName(r.name)
	params := g.pgParams()
	var head string
	if r.kind == "function" {
		ret := g.mapped(r.returns, "the result")
		head = "CREATE FUNCTION " + name + "(" + params + ") RETURNS " + ret.text
	} else {
		head = "CREATE PROCEDURE " + name + "(" + params + ")"
	}
	body := g.pgBody(r.body, nil)
	return []string{head + "\nLANGUAGE plpgsql\nAS " + dollarQuote(body)}
}

// prescan looks through the routine for what has to be known before the
// first line is written.
func (g *gen) prescan() {
	if g.r.body == nil {
		return
	}
	visitor{
		expr: func(e expr) {
			switch e := e.(type) {
			case *attrExpr:
				if e.cursor == "SQL" {
					g.usesRowCount = true
				} else {
					g.usesFound = true
				}
			case *nameExpr:
				if g.from == Postgres && len(e.parts) == 1 && strings.EqualFold(e.parts[0].name, "FOUND") {
					g.usesFound = true
				}
			}
		},
		stmt: func(s stmt) {
			if b, ok := s.(*blockStmt); ok {
				g.countCursors(b.b)
			}
		},
	}.block(g.r.body)
	g.countCursors(g.r.body)
	for _, name := range []string{rowCountVar, foundVar, messageVar, errTextVar, errStateVar} {
		if anyExpr(g.r.body, func(e expr) bool {
			n, ok := e.(*nameExpr)
			return ok && strings.EqualFold(n.parts[0].name, name)
		}) {
			g.refuse(0, "the routine uses the name %s, which the converter needs for itself", name)
		}
	}
}

func (g *gen) countCursors(b *block) {
	for _, d := range b.decls {
		if _, ok := d.(*cursorDecl); ok {
			g.cursors++
		}
	}
}

// dollarQuote wraps a routine body in a dollar quote whose tag does not
// occur in it.
func dollarQuote(body string) string {
	tag := "$body$"
	for i := 1; strings.Contains(body, tag); i++ {
		tag = fmt.Sprintf("$body%d$", i)
	}
	return tag + "\n" + body + "\n" + tag
}

// pgParams declares the parameters and writes their list.
func (g *gen) pgParams() string {
	r := g.r
	var out []string
	defaulted := false
	for _, p := range r.params {
		m := g.mapped(p.typ, "the parameter "+p.name)
		v := g.declare(&variable{name: p.name, kind: "param", typ: m.typ, dt: p.typ})
		s := v.out + " " + paramType(m.text)
		switch p.mode {
		case "OUT", "INOUT":
			if r.kind == "function" {
				g.refuse(p.line, "the function has the %s parameter %s; a PostgreSQL function with one returns a row instead of a value", p.mode, p.name)
			}
			s = p.mode + " " + s
		}
		switch {
		case p.def != nil:
			d, _ := g.value(p.def)
			s += " DEFAULT " + d
			defaulted = true
		case defaulted && p.mode == "IN":
			g.refuse(p.line, "the parameter %s has no default and follows one that has; PostgreSQL only allows defaults at the end of the list", p.name)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// paramType drops the length from a parameter's type. PostgreSQL ignores
// it there, and writing it would suggest a limit that is not enforced.
func paramType(t string) string {
	if i := strings.IndexByte(t, '('); i >= 0 && !strings.Contains(t, "%") {
		return t[:i]
	}
	return t
}

// pgBody writes the body of a function: the directive that settles what
// a name means when it is both a variable and a column, then the block.
func (g *gen) pgBody(b *block, epilogue []string) string {
	w := &writer{}
	if g.from == Oracle {
		// PL/SQL takes the column, MySQL the variable.
		w.line("#variable_conflict use_column")
	} else {
		w.line("#variable_conflict use_variable")
	}
	g.pgBlock(w, b, true, epilogue)
	return w.String()
}

// pgBlock writes a block. epilogue are lines that close the outermost
// block of a trigger function, after everything the source wrote.
func (g *gen) pgBlock(w *writer, b *block, top bool, epilogue []string) {
	g.push()
	defer g.pop()
	label := strings.ToLower(b.label)
	var errors []*handler
	var notFound *notFoundHandler
	for _, h := range b.handlers {
		isNotFound, other := false, false
		for _, c := range h.conds {
			c = g.resolveCondition(c)
			if c.kind == "notfound" || (c.kind == "sqlstate" && strings.HasPrefix(c.value, "02")) || (c.kind == "code" && c.value == "1329") {
				isNotFound = true
			} else {
				other = true
			}
		}
		switch {
		case isNotFound && other:
			g.refuse(h.line, "a handler for NOT FOUND and for errors at once")
		case isNotFound:
			if notFound == nil {
				notFound = &notFoundHandler{h: h}
			}
		case h.action == "CONTINUE":
			g.refuse(h.line, "a CONTINUE handler for errors: PL/pgSQL cannot go on with the statement after the one that failed")
		default:
			errors = append(errors, h)
		}
	}
	if notFound != nil && notFound.h.action == "EXIT" {
		if label == "" {
			g.labels++
			label = fmt.Sprintf("block%d", g.labels)
		}
		notFound.label = label
	}
	if b.label != "" || label != "" {
		g.loops = append(g.loops, &loopFrame{label: strings.ToUpper(b.label), out: label, block: true})
		defer func() { g.loops = g.loops[:len(g.loops)-1] }()
	}

	decl := &writer{indent: w.indent + 1}
	g.pgDecls(decl, b.decls)

	nested := len(errors) > 0 && len(epilogue) > 0
	body := &writer{indent: w.indent + 1}
	if nested {
		body.indent++
	}
	if notFound != nil {
		g.notFound = append(g.notFound, notFound)
	}
	if len(errors) > 0 {
		g.excDepth++
		if g.undoneByHandler(b, errors) {
			g.note(errors[0].line, "the block changes data and has an error handler; when the handler runs, PostgreSQL has undone everything the block did, where %s keeps what was done before the error", g.from)
		}
	}
	catches := catchesNoData(errors)
	if catches {
		g.noDataCaught++
	}
	g.pgStmts(body, b.body)
	if catches {
		g.noDataCaught--
	}
	if len(errors) > 0 {
		g.excDepth--
	}
	handlers := &writer{indent: body.indent - 1}
	if len(errors) > 0 {
		handlers.line("EXCEPTION")
		handlers.in()
		for i, h := range errors {
			var conds []string
			for _, c := range h.conds {
				name := g.pgCondition(c, h.line)
				if name == "others" && i != len(errors)-1 {
					g.refuse(h.line, "a handler for every error that is not the last handler of its block")
				}
				conds = append(conds, name)
			}
			handlers.line("WHEN %s THEN", strings.Join(conds, " OR "))
			handlers.in()
			g.handlerDepth++
			g.pgStmts(handlers, h.body)
			if len(h.body) == 0 {
				handlers.line("NULL;")
			}
			g.handlerDepth--
			handlers.out()
		}
	}
	if notFound != nil {
		g.notFound = g.notFound[:len(g.notFound)-1]
	}

	if label != "" {
		w.line("<<%s>>", label)
	}
	extra := ""
	if top {
		for _, line := range g.extra {
			extra += strings.Repeat("  ", w.indent+1) + line + "\n"
		}
	}
	if decl.b.Len() > 0 || extra != "" {
		w.line("DECLARE")
		w.b.WriteString(decl.b.String() + extra)
	}
	w.line("BEGIN")
	if nested {
		w.in()
		w.line("BEGIN")
		w.b.WriteString(body.b.String() + handlers.b.String())
		w.line("END;")
		w.out()
	} else {
		if body.b.Len() == 0 && len(epilogue) == 0 {
			body.line("NULL;")
		}
		w.b.WriteString(body.b.String())
	}
	w.in()
	for _, line := range epilogue {
		w.text(line)
	}
	w.out()
	if !nested {
		w.b.WriteString(handlers.b.String())
	}
	if top || label == "" {
		w.line("END;")
	} else {
		w.line("END %s;", label)
	}
}

// undoneByHandler reports whether one of a block's handlers can run
// after the block has changed data. PostgreSQL then takes the changes
// back, which Oracle and MySQL do not. It cannot when the block is one
// statement, which fails as a whole everywhere, and it cannot when the
// handlers only catch the routine's own exceptions and every RAISE of
// them comes before the first change.
func (g *gen) undoneByHandler(b *block, handlers []*handler) bool {
	if !anyStmt(b.body, changes) || countStmts(b.body) <= 1 {
		return false
	}
	own := map[string]bool{}
	for _, h := range handlers {
		for _, c := range h.conds {
			v := g.lookup(c.value)
			if c.kind != "name" || v == nil || v.kind != "exception" || v.exc.code != 0 || v.exc.cond != nil {
				return true
			}
			own[strings.ToUpper(c.value)] = true
		}
	}
	raises := func(s stmt) bool {
		r, ok := s.(*raiseStmt)
		return ok && own[r.name]
	}
	changed, unsafe := false, false
	visitor{stmt: func(s stmt) {
		switch {
		case raises(s) && changed:
			unsafe = true
		case changes(s):
			changed = true
		}
		if l, ok := s.(*loopStmt); ok && anyStmt(l.body, changes) && anyStmt(l.body, raises) {
			unsafe = true
		}
	}}.stmts(b.body)
	return unsafe
}

// pgDecls writes declarations and makes the names known.
func (g *gen) pgDecls(w *writer, decls []decl) {
	for _, d := range decls {
		switch d := d.(type) {
		case *varDecl:
			m := g.mapped(d.typ, "the variable "+d.name)
			s := g.quote(strings.ToLower(d.name))
			if d.constant {
				s += " CONSTANT"
			}
			s += " " + m.text
			if d.notNull {
				s += " NOT NULL"
			}
			if d.def != nil {
				def := g.assigned(d.def, m.typ)
				s += " := " + def
			}
			w.line("%s;", s)
			g.declare(&variable{name: d.name, kind: "var", typ: m.typ, dt: d.typ})
		case *cursorDecl:
			g.push()
			var params []string
			for _, p := range d.params {
				m := g.mapped(p.typ, "the cursor parameter "+p.name)
				v := g.declare(&variable{name: p.name, kind: "param", typ: m.typ, dt: p.typ})
				params = append(params, v.out+" "+paramType(m.text))
				if p.def != nil {
					g.refuse(p.line, "the cursor parameter %s has a default value", p.name)
				}
			}
			text, cols := g.statement(d.query, "")
			g.pop()
			v := g.declare(&variable{name: d.name, kind: "cursor", cursor: d})
			v.fields, v.anyField = g.recordFields(d.query, cols)
			if len(params) > 0 {
				w.line("%s CURSOR (%s) FOR %s;", v.out, strings.Join(params, ", "), text)
			} else {
				w.line("%s CURSOR FOR %s;", v.out, text)
			}
		case *exceptionDecl:
			g.declareException(d)
		case *unsupportedDecl:
			g.refuse(d.line, "%s cannot be converted", d.what)
		}
	}
}

// assigned writes a value that is about to be stored in something of
// the given type. Storing rounds to the type's scale, which hides the
// far digits of a division.
func (g *gen) assigned(e expr, target typ) string {
	rounds := target.class == cInt || (target.class == cNum && target.scale >= 0 && target.scale <= g.roundingLimit())
	if rounds {
		g.rounded++
		defer func() { g.rounded-- }()
	}
	s, t := g.value(e)
	if g.from == MySQL && g.to == Postgres && target.class == cStr && t.known() && t.class != cStr && t.class != cNull {
		// MySQL turns anything into text when it is stored in a text
		// variable; PL/pgSQL does too, by its own rules for each type.
		return g.asText(e, s, t, "stored as text")
	}
	return s
}

// oracleErrors are the Oracle error numbers PRAGMA EXCEPTION_INIT is
// commonly used with, as each target knows the same failure.
var oracleErrors = map[int]struct {
	pg string
	my []string
}{
	-1:    {"unique_violation", []string{"1062"}},
	100:   {"no_data_found", nil},
	-1403: {"no_data_found", nil},
	-1422: {"too_many_rows", []string{"1172"}},
	-2291: {"foreign_key_violation", []string{"1452", "1216"}},
	-2292: {"foreign_key_violation", []string{"1451", "1217"}},
	-1400: {"not_null_violation", []string{"1048"}},
	-1407: {"not_null_violation", []string{"1048"}},
	-2290: {"check_violation", []string{"3819"}},
	-60:   {"deadlock_detected", []string{"1213"}},
}

// declareException makes a named exception known. The targets have no
// named exceptions of the user's; each gets an error code instead, which
// the routine raises and its handlers catch.
func (g *gen) declareException(d *exceptionDecl) {
	v := &variable{name: d.name, kind: "exception", exc: d}
	switch {
	case d.cond != nil:
	case d.code <= -20000 && d.code >= -20999:
		v.sqlstate = fmt.Sprintf("45%03d", -d.code-20000)
	case d.code != 0:
		if _, ok := oracleErrors[d.code]; !ok {
			g.refuse(d.line, "the exception %s stands for the Oracle error %d, which the converter cannot match to an error of the target", d.name, d.code)
		}
	default:
		// The code is derived from the names, so that the exception of
		// one routine is not caught by the handler of another.
		h := fnv.New32a()
		h.Write([]byte(strings.ToUpper(g.obj.Schema + "." + g.r.name[len(g.r.name)-1].name + "." + d.name)))
		v.sqlstate = "U" + strings.ToUpper(fmt.Sprintf("%04s", strconv.FormatUint(uint64(h.Sum32()%(36*36*36*36)), 36)))
	}
	g.declare(v)
}

// resolveCondition replaces the name of a MySQL condition by what it was
// declared to stand for.
func (g *gen) resolveCondition(c condition) condition {
	if c.kind == "name" {
		if v := g.lookup(c.value); v != nil && v.kind == "exception" && v.exc.cond != nil {
			return *v.exc.cond
		}
	}
	return c
}

// oracleConditions are PL/SQL's predefined exceptions under their
// PL/pgSQL names.
var oracleConditions = map[string]string{
	"NO_DATA_FOUND": "no_data_found", "TOO_MANY_ROWS": "too_many_rows", "DUP_VAL_ON_INDEX": "unique_violation",
	"ZERO_DIVIDE": "division_by_zero", "CASE_NOT_FOUND": "case_not_found", "CURSOR_ALREADY_OPEN": "duplicate_cursor",
}

// mysqlCodes are MySQL's error numbers under their PL/pgSQL names.
var mysqlCodes = map[string]string{
	"1062": "unique_violation", "1586": "unique_violation", "1451": "foreign_key_violation", "1452": "foreign_key_violation",
	"1216": "foreign_key_violation", "1217": "foreign_key_violation", "1048": "not_null_violation", "1172": "too_many_rows",
	"3819": "check_violation", "1213": "deadlock_detected", "1365": "division_by_zero",
}

// pgCondition writes what a handler catches.
func (g *gen) pgCondition(c condition, line int) string {
	c = g.resolveCondition(c)
	switch c.kind {
	case "others":
		return "others"
	case "sqlstate":
		if c.value == "23000" && g.from == MySQL {
			// MySQL files every broken constraint under this one code.
			return "integrity_constraint_violation"
		}
		return "SQLSTATE '" + c.value + "'"
	case "code":
		if name, ok := mysqlCodes[c.value]; ok {
			return name
		}
		g.refuse(line, "a handler for the MySQL error %s, which the converter cannot match to an error of PostgreSQL", c.value)
		return "others"
	case "warning":
		g.refuse(line, "a handler for warnings: PostgreSQL does not turn warnings into conditions")
		return "others"
	}
	if v := g.lookup(c.value); v != nil && v.kind == "exception" {
		if v.sqlstate != "" {
			return "SQLSTATE '" + v.sqlstate + "'"
		}
		return oracleErrors[v.exc.code].pg
	}
	if name, ok := oracleConditions[c.value]; ok {
		return name
	}
	switch c.value {
	case "VALUE_ERROR":
		g.note(line, "VALUE_ERROR is caught as data_exception, PostgreSQL's whole class of errors in values; that class also holds division by zero, which VALUE_ERROR does not")
		return "data_exception"
	case "INVALID_NUMBER":
		g.note(line, "INVALID_NUMBER is caught as invalid_text_representation, the error PostgreSQL raises for text that is not a number")
		return "invalid_text_representation"
	}
	g.refuse(line, "a handler for %s, which has no counterpart in PostgreSQL", c.value)
	return "others"
}

// pgStmts writes a list of statements.
func (g *gen) pgStmts(w *writer, list []stmt) {
	saved := g.lastFetch
	g.lastFetch = ""
	for i, s := range list {
		var next stmt
		if i+1 < len(list) {
			next = list[i+1]
		}
		g.pgStmt(w, s, next)
		g.lastFetch = ""
		if f, ok := s.(*fetchStmt); ok {
			g.lastFetch = strings.ToUpper(f.cursor)
		}
	}
	g.lastFetch = saved
}

// target writes something that is assigned to.
func (g *gen) target(e expr) string {
	n, ok := e.(*nameExpr)
	if !ok {
		g.refuse(e.exprLine(), "%s is assigned to, and is not a variable", describeExpr(e))
		return "NULL"
	}
	saved := g.bound
	g.bound = nil
	defer func() { g.bound = saved }()
	s, _ := g.name(n)
	return s
}

func (g *gen) targetType(e expr) typ {
	n, ok := e.(*nameExpr)
	if !ok {
		return tUnknown
	}
	saved, notes, seen := g.bound, len(g.notes), len(g.refusals)
	g.bound = nil
	_, t := g.name(n)
	g.bound, g.notes, g.refusals = saved, g.notes[:notes], g.refusals[:seen]
	return t
}

func (g *gen) pgStmt(w *writer, s stmt, next stmt) {
	switch s := s.(type) {
	case *blockStmt:
		g.pgBlock(w, s.b, false, nil)
	case *assignStmt:
		w.line("%s := %s;", g.target(s.target), g.assigned(s.value, g.targetType(s.target)))
	case *ifStmt:
		for i, b := range s.branches {
			if i == 0 {
				w.line("IF %s THEN", g.cond(b.cond))
			} else {
				w.line("ELSIF %s THEN", g.cond(b.cond))
			}
			w.in()
			g.pgStmts(w, b.body)
			w.out()
		}
		if s.hasElse {
			w.line("ELSE")
			w.in()
			g.pgStmts(w, s.els)
			w.out()
		}
		w.line("END IF;")
	case *caseStmt:
		if s.operand != nil {
			x, _ := g.value(s.operand)
			w.line("CASE %s", x)
		} else {
			w.line("CASE")
		}
		w.in()
		for _, b := range s.branches {
			var vals []string
			for _, v := range b.values {
				if s.operand != nil {
					x, _ := g.value(v)
					vals = append(vals, x)
				} else {
					vals = append(vals, g.cond(v))
				}
			}
			w.line("WHEN %s THEN", strings.Join(vals, ", "))
			w.in()
			g.pgStmts(w, b.body)
			w.out()
		}
		if s.hasElse {
			w.line("ELSE")
			w.in()
			g.pgStmts(w, s.els)
			w.out()
		}
		w.out()
		w.line("END CASE;")
	case *loopStmt:
		g.pgLoop(w, s)
	case *exitStmt:
		word := "EXIT"
		if s.next {
			word = "CONTINUE"
		}
		if s.label != "" {
			word += " " + g.loopLabel(s.label, s.line)
		}
		if s.when != nil {
			word += " WHEN " + g.cond(s.when)
		}
		w.line("%s;", word)
	case *returnStmt:
		switch {
		case g.r.kind == "trigger":
			for _, line := range g.triggerReturn() {
				w.text(line)
			}
		case s.value == nil:
			w.line("RETURN;")
		default:
			ret := tUnknown
			if g.r.returns != nil {
				if m, ok := mapType(g.from, g.to, g.r.returns); ok {
					ret = m.typ
				}
			}
			w.line("RETURN %s;", g.assigned(s.value, ret))
		}
	case *nullStmt:
		w.line("NULL;")
	case *raiseStmt:
		g.pgRaise(w, s)
	case *openStmt:
		v := g.cursorVar(s.cursor, s.line)
		if len(s.args) > 0 {
			var args []string
			for _, a := range s.args {
				x, _ := g.value(a)
				args = append(args, x)
			}
			w.line("OPEN %s(%s);", v, strings.Join(args, ", "))
		} else {
			w.line("OPEN %s;", v)
		}
	case *fetchStmt:
		g.pgFetch(w, s, next)
	case *closeStmt:
		w.line("CLOSE %s;", g.cursorVar(s.cursor, s.line))
	case *sqlExec:
		g.pgSQL(w, s)
	case *callStmt:
		argNames := s.argNames
		if argNames == nil {
			argNames = make([]string, len(s.args))
		}
		text, _ := g.call(&callExpr{name: s.name, args: s.args, argNames: argNames, line: s.line})
		w.line("CALL %s;", text)
	case *outputStmt:
		x, _ := g.value(s.value)
		w.line("RAISE NOTICE '%%', %s;", x)
	case *txStmt:
		switch {
		case g.r.kind != "procedure":
			g.refuse(s.line, "%s in a %s: PostgreSQL only lets a procedure end a transaction", s.verb, g.r.kind)
		case g.excDepth > 0 || g.handlerDepth > 0:
			g.refuse(s.line, "%s inside a block with an error handler, which PostgreSQL does not allow", s.verb)
		default:
			g.note(s.line, "%s ends the transaction inside the procedure; PostgreSQL allows that only when the procedure is called outside a transaction block", s.verb)
		}
		if s.verb != "START" {
			w.line("%s;", s.verb)
		}
	case *unsupportedStmt:
		g.refuse(s.line, "%s cannot be converted", s.what)
	default:
		g.refuse(s.stmtLine(), "a statement of a kind PL/pgSQL has no form for")
	}
}

// loopLabel finds the label of an enclosing loop or block.
func (g *gen) loopLabel(label string, line int) string {
	up := strings.ToUpper(label)
	for i := len(g.loops) - 1; i >= 0; i-- {
		if g.loops[i].label == up {
			g.loops[i].used = true
			return g.loops[i].out
		}
	}
	g.refuse(line, "the label %s does not belong to a loop or block around the statement", label)
	return strings.ToLower(label)
}

func (g *gen) cursorVar(name string, line int) string {
	v := g.lookup(name)
	if v == nil || v.kind != "cursor" {
		g.refuse(line, "%s is not a cursor declared in the routine", name)
		return strings.ToLower(name)
	}
	return v.out
}

func (g *gen) pgLoop(w *writer, s *loopStmt) {
	label := strings.ToLower(s.label)
	if label != "" {
		w.line("<<%s>>", label)
	}
	g.loops = append(g.loops, &loopFrame{label: strings.ToUpper(s.label), out: label})
	g.push()
	outer := g.inLoop
	g.inLoop = s
	defer func() {
		g.inLoop = outer
		g.pop()
		g.loops = g.loops[:len(g.loops)-1]
	}()
	switch s.kind {
	case "loop", "repeat":
		w.line("LOOP")
	case "while":
		// c%FOUND as the condition is right when the round ends with
		// the next FETCH from the same cursor.
		if n := len(s.body); n > 0 {
			if f, ok := s.body[n-1].(*fetchStmt); !ok || strings.ToUpper(f.cursor) != g.lastFetch {
				g.lastFetch = ""
			}
		}
		w.line("WHILE %s LOOP", g.cond(s.cond))
	case "range":
		lo, _ := g.value(s.lo)
		hi, _ := g.value(s.hi)
		v := g.declare(&variable{name: s.variable, kind: "var", typ: tInt})
		if s.reverse {
			// PL/SQL writes the bounds low to high either way.
			w.line("FOR %s IN REVERSE %s..%s LOOP", v.out, wrap(hi, s.hi), wrap(lo, s.lo))
		} else {
			w.line("FOR %s IN %s..%s LOOP", v.out, wrap(lo, s.lo), wrap(hi, s.hi))
		}
	case "query":
		text, cols := g.statement(s.query, "")
		v := g.loopRecord(s)
		v.fields, v.anyField = g.recordFields(s.query, cols)
		w.line("FOR %s IN %s LOOP", v.out, text)
	case "cursor":
		c := g.lookup(s.cursor)
		if c == nil || c.kind != "cursor" {
			g.refuse(s.line, "%s is not a cursor declared in the routine", s.cursor)
			return
		}
		v := g.declare(&variable{name: s.variable, kind: "record", fields: c.fields, anyField: c.anyField})
		args := ""
		if len(s.args) > 0 {
			var list []string
			for _, a := range s.args {
				x, _ := g.value(a)
				list = append(list, x)
			}
			args = "(" + strings.Join(list, ", ") + ")"
		}
		w.line("FOR %s IN %s%s LOOP", v.out, c.out, args)
	}
	w.in()
	g.pgStmts(w, s.body)
	if s.kind == "repeat" {
		w.line("EXIT WHEN %s;", g.cond(s.cond))
	}
	w.out()
	if label != "" {
		w.line("END LOOP %s;", label)
	} else {
		w.line("END LOOP;")
	}
}

// loopRecord declares the record of a loop over a query. PL/SQL declares
// it by using it; PL/pgSQL wants it among the declarations.
func (g *gen) loopRecord(s *loopStmt) *variable {
	if g.declared(s.variable) {
		g.refuse(s.line, "the loop record %s has the name of something else the routine declares", s.variable)
	}
	v := g.declare(&variable{name: s.variable, kind: "record"})
	line := v.out + " record;"
	for _, e := range g.extra {
		if e == line {
			return v
		}
	}
	g.extra = append(g.extra, line)
	return v
}

// recordFields lists the fields a record gets from a query, with their
// types as far as they are known. The second result says the list is
// not complete, because the query selects with "*" from a table the
// catalog does not describe.
func (g *gen) recordFields(q *sqlStmt, cols []typ) (map[string]*field, bool) {
	fields := map[string]*field{}
	open := false
	for i, c := range q.columns {
		if star, ok := c.e.(*starExpr); ok {
			found := false
			for _, ref := range q.tables {
				if len(star.qualifier) > 0 {
					q := star.qualifier[len(star.qualifier)-1].name
					if !strings.EqualFold(ref.alias, q) && !(len(ref.name) > 0 && strings.EqualFold(ref.name[len(ref.name)-1].name, q)) {
						continue
					}
				}
				t := g.findTable(ref.name)
				if t == nil || ref.sub != nil {
					continue
				}
				found = true
				for k := range t.Columns {
					col := &t.Columns[k]
					fields[strings.ToUpper(col.Name)] = &field{out: g.columnName(col), typ: g.columnType(col)}
				}
			}
			if !found {
				open = true
			}
			continue
		}
		if c.name == "" {
			continue
		}
		f := &field{out: g.quote(g.fold(ident{name: c.name})), typ: tUnknown}
		if n, ok := c.e.(*nameExpr); ok && !hasAlias(q, c.e) {
			// An unnamed column keeps its own name on the target.
			f.out = g.ident(n.parts[len(n.parts)-1])
			for _, b := range q.tables {
				if t := g.findTable(b.name); t != nil && b.sub == nil {
					if col := t.column(g, n.parts[len(n.parts)-1]); col != nil {
						f.out = g.columnName(col)
					}
				}
			}
		}
		if i < len(cols) {
			f.typ = cols[i]
		}
		fields[strings.ToUpper(c.name)] = f
	}
	return fields, open
}

// hasAlias reports whether a select item is followed by an alias.
func hasAlias(q *sqlStmt, e expr) bool {
	for i, p := range q.parts {
		if p.e == e {
			return i+1 < len(q.parts) && q.parts[i+1].alias != nil
		}
	}
	return false
}

// fetchKeeps notes a FETCH whose variables are read after the cursor is
// exhausted. Oracle and MySQL leave the variables as the last row set
// them; PL/pgSQL sets them to NULL.
func (g *gen) fetchKeeps(s *fetchStmt, next stmt) {
	safe := g.inLoop != nil
	if safe {
		switch next.(type) {
		case *exitStmt, *ifStmt, nil:
		default:
			safe = false
		}
	}
	if safe {
		all := map[string]int{}
		visitor{expr: func(e expr) {
			if n, ok := e.(*nameExpr); ok {
				all[strings.ToUpper(n.parts[0].name)]++
			}
		}}.block(g.r.body)
		inside := references(g.inLoop.body)
		for _, t := range s.into {
			if n, ok := t.(*nameExpr); ok {
				name := strings.ToUpper(n.parts[0].name)
				if all[name] != inside[name] {
					safe = false
				}
			}
		}
	}
	if !safe {
		g.note(s.line, "after the last row, FETCH leaves its variables as they were in %s and sets them to NULL in PostgreSQL; check that they are not read once %s is exhausted", g.from, s.cursor)
	}
}

func (g *gen) pgFetch(w *writer, s *fetchStmt, next stmt) {
	var targets []string
	for _, t := range s.into {
		targets = append(targets, g.target(t))
	}
	w.line("FETCH %s INTO %s;", g.cursorVar(s.cursor, s.line), strings.Join(targets, ", "))
	g.fetchKeeps(s, next)
	if g.from != MySQL {
		return
	}
	// MySQL reports the end of a cursor as a condition; the handler in
	// force says what happens then.
	w.line("IF NOT FOUND THEN")
	w.in()
	if len(g.notFound) == 0 {
		w.line("RAISE EXCEPTION 'No data - zero rows fetched, selected, or processed' USING ERRCODE = '02000';")
	} else {
		g.runNotFound(w)
	}
	w.out()
	w.line("END IF;")
}

// runNotFound writes what the NOT FOUND handler in force does.
func (g *gen) runNotFound(w *writer) {
	h := g.notFound[len(g.notFound)-1]
	// The handler's own statements run outside its reach.
	saved := g.notFound
	g.notFound = g.notFound[:len(g.notFound)-1]
	g.pgStmts(w, h.h.body)
	g.notFound = saved
	if h.h.action == "EXIT" {
		w.line("EXIT %s;", h.label)
	} else if len(h.h.body) == 0 {
		w.line("NULL;")
	}
}

// pgSQL writes an SQL statement of the routine.
func (g *gen) pgSQL(w *writer, s *sqlExec) {
	q := s.sql
	var targets []string
	for _, t := range q.into {
		targets = append(targets, g.target(t))
	}
	into := ""
	if len(targets) > 0 {
		into = "INTO " + strings.Join(targets, ", ")
	}
	count := func() {
		if g.usesRowCount {
			w.line("GET DIAGNOSTICS %s = ROW_COUNT;", rowCountVar)
			g.needRowCount()
		}
	}
	switch {
	case q.verb != "SELECT":
		if q.returning && q.verb != "INSERT" {
			g.note(s.line, "%s ... RETURNING INTO: when no row is changed, %s leaves the variables as they were and PostgreSQL sets them to NULL", q.verb, g.from)
		}
		text, _ := g.statement(q, into)
		w.line("%s;", text)
		count()
	case len(q.into) == 0:
		g.refuse(s.line, "a SELECT whose rows go to the caller: a PostgreSQL routine has to say what it returns")
	case g.from == Oracle:
		// PL/SQL raises NO_DATA_FOUND and TOO_MANY_ROWS, which is what
		// STRICT asks of PL/pgSQL.
		g.silentNoData(s)
		text, _ := g.statement(q, "INTO STRICT "+strings.Join(targets, ", "))
		w.line("%s;", text)
		count()
	case singleRow(q):
		text, _ := g.statement(q, into)
		w.line("%s;", text)
	default:
		// MySQL leaves the variables alone when the query finds nothing
		// and tells the NOT FOUND handler. STRICT raises the error
		// before anything is assigned, and the handler here does what
		// MySQL's would.
		text, _ := g.statement(q, "INTO STRICT "+strings.Join(targets, ", "))
		w.line("BEGIN")
		w.in()
		w.line("%s;", text)
		w.out()
		w.line("EXCEPTION WHEN no_data_found THEN")
		w.in()
		if len(g.notFound) == 0 {
			w.line("NULL;")
		} else {
			g.runNotFound(w)
		}
		w.out()
		w.line("END;")
	}
}

// catchesNoData reports whether one of a block's handlers catches the
// error of a query that found no row.
func catchesNoData(handlers []*handler) bool {
	for _, h := range handlers {
		for _, c := range h.conds {
			if c.kind == "others" || (c.kind == "name" && c.value == "NO_DATA_FOUND") {
				return true
			}
		}
	}
	return false
}

// silentNoData notes a query in an Oracle function that can find no row
// without the function handling it. Oracle raises NO_DATA_FOUND, and an
// SQL statement that called the function quietly takes that for a NULL
// result; the targets report an error.
func (g *gen) silentNoData(s *sqlExec) {
	if g.r.kind == "function" && g.noDataCaught == 0 && !singleRow(s.sql) {
		g.note(s.line, "if this query finds no row the function raises NO_DATA_FOUND; an Oracle SQL statement calling the function reads that as NULL, where %s reports an error", g.to)
	}
}

// needRowCount adds the variable that holds the row count of the last
// statement.
func (g *gen) needRowCount() {
	line := rowCountVar + " bigint;"
	for _, e := range g.extra {
		if e == line {
			return
		}
	}
	g.extra = append(g.extra, line)
}

func (g *gen) pgRaise(w *writer, s *raiseStmt) {
	switch {
	case s.reraise:
		if g.handlerDepth == 0 {
			g.refuse(s.line, "an error is passed on outside an error handler")
		}
		w.line("RAISE;")
	case s.oraCode != nil:
		code, ok := intLit(s.oraCode)
		if !ok || code > -20000 || code < -20999 {
			g.refuse(s.line, "RAISE_APPLICATION_ERROR with an error number that is not a literal between -20999 and -20000")
			return
		}
		msg, _ := g.value(s.message)
		w.line("RAISE EXCEPTION '%%', %s USING ERRCODE = '45%03d';", msg, -code-20000)
	case s.sqlstate != "":
		if strings.HasPrefix(s.sqlstate, "01") || strings.HasPrefix(s.sqlstate, "02") {
			g.refuse(s.line, "SIGNAL of a warning or of NOT FOUND (SQLSTATE %s)", s.sqlstate)
			return
		}
		msg := "'Unhandled user-defined exception condition'"
		if s.message != nil {
			msg, _ = g.value(s.message)
		}
		w.line("RAISE EXCEPTION '%%', %s USING ERRCODE = '%s';", msg, s.sqlstate)
	default:
		v := g.lookup(s.name)
		switch {
		case v != nil && v.kind == "exception" && v.exc.cond != nil:
			c := *v.exc.cond
			if c.kind != "sqlstate" {
				g.refuse(s.line, "SIGNAL of the condition %s, which stands for an error number", s.name)
				return
			}
			g.pgRaise(w, &raiseStmt{sqlstate: c.value, message: s.message, line: s.line})
		case v != nil && v.kind == "exception" && v.sqlstate != "":
			w.line("RAISE EXCEPTION USING ERRCODE = '%s', MESSAGE = %s;", v.sqlstate, g.stringLit(strings.ToLower(v.name)))
		case v != nil && v.kind == "exception":
			w.line("RAISE %s;", oracleErrors[v.exc.code].pg)
		default:
			name, ok := oracleConditions[s.name]
			if !ok {
				g.refuse(s.line, "RAISE %s: the exception is not declared in the routine and has no counterpart in PostgreSQL", s.name)
				return
			}
			w.line("RAISE %s;", name)
		}
	}
}

// Triggers.

// triggerReturn is how a trigger function ends. A row trigger that
// fires before the change hands the row back, which is what lets the
// change go ahead; for any other trigger the result is ignored.
func (g *gen) triggerReturn() []string {
	h := g.r.trigger
	if h.timing != "BEFORE" || !h.forEachRow {
		return []string{"RETURN NULL;"}
	}
	del, other := false, false
	for _, e := range h.events {
		if e == "DELETE" {
			del = true
		} else {
			other = true
		}
	}
	switch {
	case del && other:
		return []string{"IF TG_OP = 'DELETE' THEN", "  RETURN OLD;", "END IF;", "RETURN NEW;"}
	case del:
		return []string{"RETURN OLD;"}
	}
	return []string{"RETURN NEW;"}
}

func (g *gen) pgTrigger() []string {
	h := g.r.trigger
	if h.timing == "INSTEAD OF" {
		g.refuse(h.line, "an INSTEAD OF trigger: it belongs to a view, and views are migrated separately")
		return nil
	}
	name := g.fold(h.name[len(h.name)-1])
	fn := name
	if len(fn) > 60 {
		fn = fn[:60]
	}
	fn = g.qualify(g.quote(fn + "_fn"))
	body := g.pgBody(g.r.body, g.triggerReturn())

	head := "CREATE TRIGGER " + g.quote(name) + " " + h.timing + " "
	var events []string
	for _, e := range h.events {
		if e == "UPDATE" && len(h.updateOf) > 0 {
			var cols []string
			table := g.findTable(h.table)
			for _, c := range h.updateOf {
				if table != nil {
					if col := table.column(g, c); col != nil {
						cols = append(cols, g.columnName(col))
						continue
					}
				}
				cols = append(cols, g.ident(c))
			}
			e += " OF " + strings.Join(cols, ", ")
		}
		events = append(events, e)
	}
	head += strings.Join(events, " OR ") + " ON " + g.tableName(h.table)
	if h.forEachRow {
		head += "\nFOR EACH ROW"
	} else {
		head += "\nFOR EACH STATEMENT"
	}
	if h.when != nil {
		uses := func(row string) bool {
			found := false
			visitor{expr: func(e expr) {
				if n, ok := e.(*nameExpr); ok && strings.ToUpper(n.parts[0].name) == row {
					found = true
				}
			}}.e(h.when)
			return found
		}
		for _, e := range h.events {
			if (e == "DELETE" && uses(h.newName)) || (e == "INSERT" && uses(h.oldName)) {
				g.refuse(h.line, "the WHEN clause reads a row the trigger does not have on %s; PostgreSQL rejects that where Oracle reads NULLs", e)
			}
		}
		g.inWhen = true
		head += " WHEN (" + g.cond(h.when) + ")"
		g.inWhen = false
	}
	head += "\nEXECUTE FUNCTION " + fn + "()"
	return []string{
		"CREATE FUNCTION " + fn + "() RETURNS trigger\nLANGUAGE plpgsql\nAS " + dollarQuote(body),
		head,
	}
}
