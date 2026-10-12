package codesql

import (
	"fmt"
	"strings"
)

// The label the emitter gives the outermost block when a statement has
// to leave it: MySQL has no RETURN in a procedure or a trigger.
const bodyLabel = "routine_body"

// noDataState is the SQLSTATE the converted code raises when a query
// that must find a row finds none. MySQL only warns in that case; Oracle
// and PL/pgSQL's INTO STRICT raise an error. The code is PostgreSQL's
// own for it.
const noDataState = "P0002"

// emitMySQL writes the routine as a MySQL stored program and returns the
// statements that create it: one, or one for each event of a trigger
// that fires on several, because a MySQL trigger has a single event.
func (g *gen) emitMySQL() []string {
	r := g.r
	g.prescan()
	if r.conflict == "use_column" {
		g.refuse(0, "the routine asks for columns to win over variables of the same name (#variable_conflict use_column); in MySQL the variable always wins")
	}
	if r.kind == "trigger" {
		return g.myTrigger()
	}
	g.push()
	defer g.pop()
	name := g.objectName(r.name)
	params, prologue := g.myParams()
	if r.voidFunction {
		g.note(0, "the function returns void and becomes a procedure; MySQL calls it with CALL, not from a query")
	}
	if r.kind == "procedure" {
		body := g.myBody(r.body, prologue)
		return []string{"CREATE PROCEDURE " + name + "(" + params + ")\n" + body}
	}
	ret := g.mapped(r.returns, "the result")
	body := g.myBody(r.body, prologue)
	return []string{"CREATE FUNCTION " + name + "(" + params + ") RETURNS " + ret.text + "\n    " + g.characteristics() + "\n" + body}
}

// characteristics says what MySQL wants to know about a function before
// it lets one be created while the binary log is on: whether it reads or
// changes data, and whether it always gives the same result.
func (g *gen) characteristics() string {
	modifies := anyStmt(g.r.body.body, changes)
	for _, h := range g.r.body.handlers {
		modifies = modifies || anyStmt(h.body, changes)
	}
	reading := false
	visitor{
		stmt: func(s stmt) {
			switch s := s.(type) {
			case *sqlExec:
				reading = reading || reads(s.sql)
			case *loopStmt:
				reading = reading || s.kind == "query" || s.kind == "cursor"
			case *blockStmt:
				for _, d := range s.b.decls {
					if _, ok := d.(*cursorDecl); ok {
						reading = true
					}
				}
			}
		},
		expr: func(e expr) {
			switch e.(type) {
			case *subExpr, *existsExpr:
				reading = true
			case *inExpr:
				reading = reading || e.(*inExpr).sub != nil
			}
		},
	}.block(g.r.body)
	for _, d := range g.r.body.decls {
		if _, ok := d.(*cursorDecl); ok {
			reading = true
		}
	}
	var out []string
	if g.r.deterministic {
		out = append(out, "DETERMINISTIC")
	}
	switch {
	case modifies:
		g.note(0, "the function changes data; while the binary log is on, MySQL only creates such a function when log_bin_trust_function_creators is set")
		out = append(out, "MODIFIES SQL DATA")
	case reading:
		out = append(out, "READS SQL DATA")
	default:
		out = append(out, "NO SQL")
	}
	return strings.Join(out, " ")
}

// myParams declares the parameters and writes their list, and the
// statements that stand in for what MySQL's parameters cannot say.
func (g *gen) myParams() (string, []string) {
	r := g.r
	var out, strict, prologue []string
	for _, p := range r.params {
		m := g.mapped(p.typ, "the parameter "+p.name)
		v := g.declare(&variable{name: p.name, kind: "param", typ: m.typ, dt: p.typ})
		s := v.out + " " + m.text
		switch {
		case r.kind == "function" && p.mode != "IN":
			g.refuse(p.line, "the function has the %s parameter %s; a MySQL function only takes values in", p.mode, p.name)
		case r.kind == "procedure":
			s = p.mode + " " + s
		}
		if p.def != nil {
			g.note(p.line, "the parameter %s has a default value, which MySQL does not support; every call has to pass it", p.name)
		}
		if p.mode == "IN" {
			strict = append(strict, v.out+" IS NULL")
		}
		out = append(out, s)
	}
	if r.strict && r.kind == "function" && len(strict) > 0 {
		// PostgreSQL does not run a STRICT function for a NULL argument.
		prologue = append(prologue, "IF "+strings.Join(strict, " OR ")+" THEN", "  RETURN NULL;", "END IF;")
	}
	return strings.Join(out, ", "), prologue
}

// myBody writes the outermost block.
func (g *gen) myBody(b *block, prologue []string) string {
	w := &writer{}
	g.myBlock(w, b, true, prologue)
	return strings.TrimSuffix(w.String(), ";")
}

// classDecl is the MySQL type for a value of which only the class is
// known: the field of a record that is computed rather than read from a
// column.
func (g *gen) classDecl(t typ) (string, bool) {
	switch t.class {
	case cInt:
		return "BIGINT", true
	case cNum:
		if t.scale >= 0 && t.scale <= 30 {
			return fmt.Sprintf("DECIMAL(65,%d)", t.scale), true
		}
		return "DECIMAL(65,30)", true
	case cFloat:
		return "DOUBLE", true
	case cStr:
		return "LONGTEXT CHARACTER SET utf8mb4 COLLATE " + g.opt.Collation, true
	case cDate:
		return "DATE", true
	case cTime, cTimeTZ:
		return "DATETIME(6)", true
	case cBool:
		return "TINYINT(1)", true
	}
	return "", false
}

// columnDecl is the MySQL type of a catalog column.
func (g *gen) columnDecl(c *Column) (string, bool) {
	dt, ok := parseTypeText(g.from, c.Type)
	if !ok {
		return "", false
	}
	m, ok := mapType(g.from, g.to, dt)
	if !ok {
		return "", false
	}
	if m.text8 {
		m.text += " CHARACTER SET utf8mb4 COLLATE " + g.opt.Collation
	}
	return m.text, true
}

// myBlock writes a block. MySQL wants a block's declarations in a fixed
// order: variables, then cursors, then handlers.
func (g *gen) myBlock(w *writer, b *block, top bool, prologue []string) {
	g.push()
	defer g.pop()
	label := strings.ToLower(b.label)
	if b.label != "" {
		g.loops = append(g.loops, &loopFrame{label: strings.ToUpper(b.label), out: g.quote(label), block: true, depth: g.handlerDepth})
		defer func() { g.loops = g.loops[:len(g.loops)-1] }()
	}
	vars := &writer{indent: w.indent + 1}
	cursors := &writer{indent: w.indent + 1}
	handlers := &writer{indent: w.indent + 1}
	g.myDecls(vars, cursors, b.decls)

	body := &writer{indent: w.indent + 1}
	for _, line := range prologue {
		body.text(line)
	}
	if len(b.handlers) > 0 {
		g.excDepth++
		if g.from == Postgres && g.undoneByHandler(b, b.handlers) {
			g.note(b.handlers[0].line, "the block changes data and has an error handler; when the handler runs, PostgreSQL has undone everything the block did, where MySQL keeps what was done before the error")
		}
	}
	catches := catchesNoData(b.handlers)
	if catches {
		g.noDataCaught++
	}
	g.myStmts(body, b.body, top)
	if catches {
		g.noDataCaught--
	}
	if len(b.handlers) > 0 {
		g.excDepth--
	}
	for i, h := range b.handlers {
		var conds []string
		for _, c := range h.conds {
			names := g.myCondition(c, h.line)
			if c.kind == "others" && i != len(b.handlers)-1 {
				g.refuse(h.line, "a handler for every error that is not the last handler of its block: MySQL picks the most specific handler, not the first")
			}
			conds = append(conds, names...)
		}
		hw := &writer{indent: handlers.indent + 1}
		g.handlerDepth++
		g.usesErrText, g.usesErrState = false, false
		outerTop := g.handlerTop
		g.handlerTop = top
		// When the handler of the outermost block is done the routine
		// ends, so a RETURN that closes it needs no statement.
		g.myStmts(hw, h.body, top && g.r.kind != "function")
		g.handlerTop = outerTop
		g.handlerDepth--
		handlers.line("DECLARE EXIT HANDLER FOR %s", strings.Join(conds, ", "))
		handlers.line("BEGIN")
		if g.usesErrText || g.usesErrState {
			handlers.line("  GET DIAGNOSTICS CONDITION 1 %s = MESSAGE_TEXT, %s = RETURNED_SQLSTATE;", errTextVar, errStateVar)
			g.needs(errTextVar+" TEXT", errStateVar+" CHAR(5)")
		}
		handlers.b.WriteString(hw.b.String())
		handlers.line("END;")
	}
	if top {
		for _, d := range g.extra {
			vars.line("DECLARE %s;", d)
		}
		if g.hasFetch {
			if g.usesFound {
				handlers.line("DECLARE CONTINUE HANDLER FOR NOT FOUND SET %s = 0;", foundVar)
			} else {
				// Reaching the end of a cursor is not an error in the
				// source language.
				handlers.line("DECLARE CONTINUE HANDLER FOR NOT FOUND BEGIN END;")
			}
		}
		if g.needBodyLabel {
			if label != "" {
				g.refuse(b.line, "the outermost block has a label of its own and the routine leaves it early")
			}
			label = bodyLabel
		}
	}
	if label != "" {
		w.line("%s: BEGIN", g.quote(label))
	} else {
		w.line("BEGIN")
	}
	w.b.WriteString(vars.b.String() + cursors.b.String() + handlers.b.String() + body.b.String())
	w.line("END;")
}

// needs adds a variable to the outermost block, once.
func (g *gen) needs(decls ...string) {
	for _, d := range decls {
		have := false
		for _, e := range g.extra {
			have = have || e == d
		}
		if !have {
			g.extra = append(g.extra, d)
		}
	}
}

func (g *gen) myDecls(vars, cursors *writer, decls []decl) {
	for _, d := range decls {
		switch d := d.(type) {
		case *varDecl:
			if d.typ != nil && d.typ.name == "RECORD" && len(d.typ.ref) == 0 {
				// PL/pgSQL declares the record of a FOR loop; the loop
				// itself brings its variables. Any other use of the
				// record is refused where it occurs.
				g.declare(&variable{name: d.name, kind: "record", anyField: true})
				continue
			}
			m := g.mapped(d.typ, "the variable "+d.name)
			s := "DECLARE " + g.quote(strings.ToLower(d.name)) + " " + m.text
			if d.notNull {
				g.note(d.line, "the variable %s is declared NOT NULL, which MySQL cannot enforce", d.name)
			}
			if d.def != nil {
				s += " DEFAULT " + g.assigned(d.def, m.typ)
			}
			vars.line("%s;", s)
			g.declare(&variable{name: d.name, kind: "var", typ: m.typ, dt: d.typ})
		case *cursorDecl:
			if len(d.params) > 0 {
				g.refuse(d.line, "the cursor %s takes parameters, which MySQL cursors cannot", d.name)
			}
			text, cols := g.statement(d.query, "")
			v := g.declare(&variable{name: d.name, kind: "cursor", cursor: d})
			v.fields, v.anyField = g.recordFields(d.query, cols)
			cursors.line("DECLARE %s CURSOR FOR %s;", v.out, text)
		case *exceptionDecl:
			g.declareException(d)
		case *unsupportedDecl:
			g.refuse(d.line, "%s cannot be converted", d.what)
		}
	}
}

// pgErrorClasses are the classes of SQLSTATE PostgreSQL uses for its
// own errors. A handler for a code in one of them waits for an error
// MySQL reports under another code, if at all.
var pgErrorClasses = words("03 08 09 0A 0B 0F 0L 0P 0Z 20 21 22 23 24 25 26 27 28 2B 2D 2F 34 38 39 3B 3D 3F 40 42 44 53 54 55 57 58 72 F0 HV XX")

// pgConditions are PostgreSQL's errors as MySQL knows the same failure.
var pgConditions = map[string][]string{
	"NO_DATA_FOUND": {"SQLSTATE '" + noDataState + "'"}, "P0002": {"SQLSTATE '" + noDataState + "'"},
	"TOO_MANY_ROWS": {"1172"}, "P0003": {"1172"},
	"UNIQUE_VIOLATION": {"1062"}, "23505": {"1062"},
	"FOREIGN_KEY_VIOLATION": {"1451", "1452"}, "23503": {"1451", "1452"},
	"NOT_NULL_VIOLATION": {"1048"}, "23502": {"1048"},
	"CHECK_VIOLATION": {"3819"}, "23514": {"3819"},
	"RAISE_EXCEPTION": {"SQLSTATE 'P0001'"}, "P0001": {"SQLSTATE 'P0001'"},
	"DEADLOCK_DETECTED": {"1213"}, "40P01": {"1213"},
}

// oracleMyConditions are PL/SQL's predefined exceptions as MySQL knows
// the same failure.
var oracleMyConditions = map[string][]string{
	"NO_DATA_FOUND": {"SQLSTATE '" + noDataState + "'"}, "TOO_MANY_ROWS": {"1172"}, "DUP_VAL_ON_INDEX": {"1062"},
}

// myCondition writes what a handler catches.
func (g *gen) myCondition(c condition, line int) []string {
	switch c.kind {
	case "others":
		return []string{"SQLEXCEPTION"}
	case "sqlstate":
		if names, ok := pgConditions[c.value]; ok {
			return names
		}
		if len(c.value) == 5 && pgErrorClasses[c.value[:2]] {
			g.refuse(line, "a handler for SQLSTATE %s, an error of PostgreSQL's own that the converter cannot match to an error of MySQL", c.value)
		}
		return []string{"SQLSTATE '" + c.value + "'"}
	}
	if v := g.lookup(c.value); v != nil && v.kind == "exception" {
		if v.sqlstate != "" {
			return []string{"SQLSTATE '" + v.sqlstate + "'"}
		}
		if codes := oracleErrors[v.exc.code].my; len(codes) > 0 {
			return codes
		}
		g.refuse(line, "a handler for %s, an Oracle error MySQL has no single counterpart for", c.value)
		return []string{"SQLEXCEPTION"}
	}
	table := pgConditions
	if g.from == Oracle {
		table = oracleMyConditions
	}
	if names, ok := table[c.value]; ok {
		return names
	}
	switch c.value {
	case "DIVISION_BY_ZERO", "ZERO_DIVIDE":
		g.refuse(line, "a handler for division by zero: MySQL gives NULL instead of raising an error")
	default:
		name := c.value
		if g.from == Postgres {
			name = strings.ToLower(name)
		}
		g.refuse(line, "a handler for %s, which has no counterpart in MySQL", name)
	}
	return []string{"SQLEXCEPTION"}
}

// myStmts writes a list of statements. MySQL does not accept an empty
// list where the source languages take NULL, so an empty one becomes an
// empty block. tail says the list is the body of the outermost block,
// whose last statement ends the routine anyway.
//
// It reports whether the list always ends the routine: it ran into a
// RETURN that is not inside a condition. What follows such a statement
// is never run and is not written; in a trigger written once for each
// event that is how the code for the other events disappears.
func (g *gen) myStmts(w *writer, list []stmt, tail bool) bool {
	before := w.b.Len()
	saved := g.lastFetch
	g.lastFetch = ""
	ends := false
	for i, s := range list {
		var next stmt
		if i+1 < len(list) {
			next = list[i+1]
		}
		ends = g.myStmt(w, s, next, tail && (i == len(list)-1 || g.decided(s)))
		if ends {
			break
		}
		g.lastFetch = ""
		if f, ok := s.(*fetchStmt); ok {
			g.lastFetch = strings.ToUpper(f.cursor)
		}
	}
	g.lastFetch = saved
	if w.b.Len() == before {
		w.line("BEGIN END;")
	}
	return ends
}

// decided reports whether a statement is an IF whose outcome is known
// while converting, so that it is replaced by one of its branches.
func (g *gen) decided(s stmt) bool {
	i, ok := s.(*ifStmt)
	if !ok {
		return false
	}
	for _, b := range i.branches {
		value, known := g.foldCond(b.cond)
		if !known {
			return false
		}
		if value {
			return true
		}
	}
	return true
}

// branch is one arm of an IF that is still to be written.
type branch struct {
	cond expr
	body []stmt
}

// myStmt writes one statement and reports whether it always ends the
// routine. last says nothing follows it before the routine's end.
func (g *gen) myStmt(w *writer, s stmt, next stmt, last bool) bool {
	switch s := s.(type) {
	case *blockStmt:
		g.myBlock(w, s.b, false, nil)
	case *assignStmt:
		w.line("SET %s = %s;", g.target(s.target), g.assigned(s.value, g.targetType(s.target)))
	case *ifStmt:
		return g.myIf(w, s, last)
	case *caseStmt:
		if s.operand != nil {
			x, _ := g.value(s.operand)
			w.line("CASE %s", x)
		} else {
			w.line("CASE")
		}
		w.in()
		for _, b := range s.branches {
			// MySQL takes one value for each WHEN.
			for _, v := range b.values {
				if s.operand != nil {
					x, _ := g.value(v)
					w.line("WHEN %s THEN", x)
				} else {
					w.line("WHEN %s THEN", g.cond(v))
				}
				w.in()
				g.myStmts(w, b.body, false)
				w.out()
			}
		}
		if s.hasElse {
			w.line("ELSE")
			w.in()
			g.myStmts(w, s.els, false)
			w.out()
		}
		w.out()
		w.line("END CASE;")
	case *loopStmt:
		g.myLoop(w, s)
	case *exitStmt:
		g.myExit(w, s)
	case *returnStmt:
		g.myReturn(w, s, last)
		return true
	case *nullStmt:
	case *raiseStmt:
		g.myRaise(w, s)
	case *openStmt:
		if len(s.args) > 0 {
			g.refuse(s.line, "the cursor %s is opened with arguments, which MySQL cursors cannot take", s.cursor)
		}
		w.line("OPEN %s;", g.cursorVar(s.cursor, s.line))
	case *fetchStmt:
		var targets []string
		for _, t := range s.into {
			targets = append(targets, g.target(t))
		}
		g.hasFetch = true
		if g.usesFound {
			g.needs(foundVar + " TINYINT DEFAULT 0")
			w.line("SET %s = 1;", foundVar)
		}
		w.line("FETCH %s INTO %s;", g.cursorVar(s.cursor, s.line), strings.Join(targets, ", "))
		if g.from == Postgres {
			g.fetchKeeps(s, next)
		}
	case *closeStmt:
		w.line("CLOSE %s;", g.cursorVar(s.cursor, s.line))
	case *sqlExec:
		g.mySQL(w, s)
	case *callStmt:
		argNames := s.argNames
		if argNames == nil {
			argNames = make([]string, len(s.args))
		}
		text, _ := g.call(&callExpr{name: s.name, args: s.args, argNames: argNames, line: s.line})
		w.line("CALL %s;", text)
	case *outputStmt:
		g.value(s.value)
		g.note(s.line, "DBMS_OUTPUT.PUT_LINE is dropped: MySQL has nowhere to send a message from a routine")
	case *txStmt:
		switch {
		case g.r.kind != "procedure":
			g.refuse(s.line, "%s in a %s, which MySQL does not allow", s.verb, g.r.kind)
		default:
			g.note(s.line, "%s ends the transaction inside the procedure; MySQL then starts the next one by its autocommit setting, so check what the statements after it belong to", s.verb)
		}
		w.line("%s;", s.verb)
	case *diagStmt:
		g.usesRowCount = true
		g.needs(rowCountVar + " BIGINT")
		w.line("SET %s = %s;", g.target(s.target), rowCountVar)
	case *performStmt:
		x, _ := g.value(s.value)
		w.line("DO %s;", x)
	case *unsupportedStmt:
		g.refuse(s.line, "%s cannot be converted", s.what)
	default:
		g.refuse(s.stmtLine(), "a statement of a kind MySQL has no form for")
	}
	return false
}

// myIf writes an IF. In a trigger written once for each event, a branch
// that tests the event is decided here: MySQL does not even accept the
// row a trigger does not have in code that never runs.
func (g *gen) myIf(w *writer, s *ifStmt, last bool) bool {
	var live []branch
	els, hasElse := s.els, s.hasElse
	for _, b := range s.branches {
		if value, known := g.foldCond(b.cond); known {
			if value {
				els, hasElse = b.body, true
				break
			}
			continue
		}
		live = append(live, branch{g.simplify(b.cond), b.body})
	}
	if len(live) == 0 {
		if hasElse {
			return g.myStmts(w, els, last)
		}
		return false
	}
	for i, b := range live {
		if i == 0 {
			w.line("IF %s THEN", g.cond(b.cond))
		} else {
			w.line("ELSEIF %s THEN", g.cond(b.cond))
		}
		w.in()
		g.myStmts(w, b.body, last)
		w.out()
	}
	if hasElse {
		w.line("ELSE")
		w.in()
		g.myStmts(w, els, last)
		w.out()
	}
	w.line("END IF;")
	return false
}

// foldCond decides a condition that only asks which event a trigger is
// firing for: TG_OP = 'INSERT', INSERTING, and what AND, OR and NOT make
// of them.
func (g *gen) foldCond(e expr) (value, known bool) {
	if g.event == "" {
		return false, false
	}
	isOp := func(x expr) bool {
		n, ok := x.(*nameExpr)
		return ok && len(n.parts) == 1 && strings.EqualFold(n.parts[0].name, "TG_OP") && g.from == Postgres
	}
	switch e := e.(type) {
	case *parenExpr:
		if len(e.items) == 1 {
			return g.foldCond(e.items[0])
		}
	case *nameExpr:
		if g.from == Oracle && len(e.parts) == 1 && !e.bind {
			switch strings.ToUpper(e.parts[0].name) {
			case "INSERTING":
				return g.event == "INSERT", true
			case "UPDATING":
				return g.event == "UPDATE", true
			case "DELETING":
				return g.event == "DELETE", true
			}
		}
	case *unExpr:
		if e.op == "NOT" {
			v, ok := g.foldCond(e.x)
			return !v, ok
		}
	case *binExpr:
		switch e.op {
		case "=", "<>":
			lit, other := e.r, e.l
			if isOp(e.r) {
				lit, other = e.l, e.r
			}
			if text, ok := isLit(lit, "string"); ok && isOp(other) {
				return (text == g.event) == (e.op == "="), true
			}
		case "AND", "OR":
			lv, lk := g.foldCond(e.l)
			rv, rk := g.foldCond(e.r)
			switch {
			case lk && rk && e.op == "AND":
				return lv && rv, true
			case lk && rk:
				return lv || rv, true
			case e.op == "AND" && ((lk && !lv) || (rk && !rv)):
				return false, true
			case e.op == "OR" && ((lk && lv) || (rk && rv)):
				return true, true
			}
		}
	case *inExpr:
		if isOp(e.x) && e.sub == nil {
			found := false
			for _, it := range e.list {
				text, ok := isLit(it, "string")
				if !ok {
					return false, false
				}
				found = found || text == g.event
			}
			return found != e.not, true
		}
	}
	return false, false
}

// simplify drops from a condition the parts that are already decided:
// "TG_OP = 'UPDATE' AND x" is just x in the trigger for UPDATE.
func (g *gen) simplify(e expr) expr {
	b, ok := e.(*binExpr)
	if !ok || (b.op != "AND" && b.op != "OR") {
		return e
	}
	neutral := b.op == "AND"
	if v, known := g.foldCond(b.l); known && v == neutral {
		return g.simplify(b.r)
	}
	if v, known := g.foldCond(b.r); known && v == neutral {
		return g.simplify(b.l)
	}
	return e
}

// newLabel makes up a label for a loop that has none.
func (g *gen) newLabel(prefix string) string {
	g.labels++
	return fmt.Sprintf("%s%d", prefix, g.labels)
}

func (g *gen) myLoop(w *writer, s *loopStmt) {
	label := strings.ToLower(s.label)
	if label == "" {
		label = g.newLabel("loop")
	}
	frame := &loopFrame{label: strings.ToUpper(s.label), out: g.quote(label), depth: g.handlerDepth}
	g.loops = append(g.loops, frame)
	g.push()
	outer := g.inLoop
	g.inLoop = s
	defer func() {
		g.inLoop = outer
		g.pop()
		g.loops = g.loops[:len(g.loops)-1]
	}()
	body := func() {
		w.in()
		g.myStmts(w, s.body, false)
		w.out()
	}
	switch s.kind {
	case "loop":
		w.line("%s: LOOP", frame.out)
		body()
		w.line("END LOOP;")
	case "while":
		if n := len(s.body); n > 0 {
			if f, ok := s.body[n-1].(*fetchStmt); !ok || strings.ToUpper(f.cursor) != g.lastFetch {
				g.lastFetch = ""
			}
		}
		w.line("%s: WHILE %s DO", frame.out, g.cond(s.cond))
		body()
		w.line("END WHILE;")
	case "range":
		g.myRange(w, s, frame)
	case "query", "cursor":
		g.myRecordLoop(w, s, frame)
	}
}

// myRange writes a FOR loop over a range of integers, which MySQL does
// not have: a variable, a limit computed once, and a WHILE.
func (g *gen) myRange(w *writer, s *loopStmt, frame *loopFrame) {
	lo, _ := g.value(s.lo)
	hi, _ := g.value(s.hi)
	step := "1"
	if s.step != nil {
		if _, ok := intLit(s.step); !ok {
			g.refuse(s.line, "a FOR loop whose step is not a literal number")
		}
		step, _ = g.value(s.step)
	}
	if anyStmt(s.body, func(st stmt) bool {
		a, ok := st.(*assignStmt)
		if !ok {
			return false
		}
		n, ok := a.target.(*nameExpr)
		return ok && len(n.parts) == 1 && strings.EqualFold(n.parts[0].name, s.variable)
	}) {
		g.refuse(s.line, "the loop variable %s is assigned to inside its loop", s.variable)
	}
	v := g.declare(&variable{name: s.variable, kind: "var", typ: tInt})
	end := g.quote(strings.ToLower(s.variable) + "_end")
	start, limit, test, move := lo, hi, "<=", "+"
	if s.reverse {
		start, limit, test, move = hi, lo, ">=", "-"
	}
	w.line("BEGIN")
	w.in()
	w.line("DECLARE %s INT DEFAULT %s;", v.out, start)
	w.line("DECLARE %s INT DEFAULT %s;", end, limit)
	w.line("%s: WHILE %s %s %s DO", frame.out, v.out, test, end)
	w.in()
	// CONTINUE must still reach the step, so the body becomes a block
	// that CONTINUE leaves.
	skips := continues(s.body, s.label, false)
	if skips {
		frame.nextLabel = g.quote(g.newLabel("next"))
		w.line("%s: BEGIN", frame.nextLabel)
		w.in()
	}
	g.myStmts(w, s.body, false)
	if skips {
		w.out()
		w.line("END;")
	}
	w.line("SET %s = %s %s %s;", v.out, v.out, move, step)
	w.out()
	w.line("END WHILE;")
	w.out()
	w.line("END;")
}

// myRecordLoop writes a FOR loop over a query or a cursor. MySQL has
// neither the loop nor the record: the record becomes one variable for
// each column, and the loop a cursor that is fetched until it is empty.
func (g *gen) myRecordLoop(w *writer, s *loopStmt, frame *loopFrame) {
	var fields map[string]*field
	var open bool
	var order []string
	cursor := ""
	var query string
	if s.kind == "query" {
		var cols []typ
		query, cols = g.statement(s.query, "")
		fields, open = g.recordFields(s.query, cols)
		for _, c := range s.query.columns {
			order = append(order, strings.ToUpper(c.name))
		}
		if hasStar(s.query) {
			order = g.starOrder(s.query)
		}
	} else {
		c := g.lookup(s.cursor)
		if c == nil || c.kind != "cursor" {
			g.refuse(s.line, "%s is not a cursor declared in the routine", s.cursor)
			return
		}
		if len(s.args) > 0 {
			g.refuse(s.line, "the cursor %s is opened with arguments, which MySQL cursors cannot take", s.cursor)
		}
		fields, open, cursor = c.fields, c.anyField, c.out
		for _, col := range c.cursor.query.columns {
			order = append(order, strings.ToUpper(col.name))
		}
		if hasStar(c.cursor.query) {
			order = g.starOrder(c.cursor.query)
		}
	}
	if open {
		g.refuse(s.line, "the loop reads every column (*) of a table the converter was not told about, and MySQL needs a variable for each column")
		return
	}
	rec := strings.ToLower(s.variable)
	v := g.declare(&variable{name: s.variable, kind: "record", fields: map[string]*field{}})
	done := g.quote(rec + "_done")
	w.line("BEGIN")
	w.in()
	var targets []string
	for _, name := range order {
		f := fields[name]
		if name == "" || f == nil {
			g.refuse(s.line, "a column of the loop's query has no name, so the record has no field for it; give it an alias")
			continue
		}
		if f.decl == "" {
			g.refuse(s.line, "the type of %s.%s is not known, and MySQL needs a variable of a stated type for it", s.variable, strings.ToLower(name))
			continue
		}
		out := g.quote(rec + "_" + strings.ToLower(name))
		v.fields[name] = &field{out: out, typ: f.typ, decl: f.decl}
		w.line("DECLARE %s %s;", out, f.decl)
		targets = append(targets, out)
	}
	w.line("DECLARE %s TINYINT DEFAULT 0;", done)
	if s.kind == "query" {
		cursor = g.quote(rec + "_cursor")
		w.line("DECLARE %s CURSOR FOR %s;", cursor, query)
	}
	if g.usesFound {
		w.line("DECLARE CONTINUE HANDLER FOR NOT FOUND SET %s = 1, %s = 0;", done, foundVar)
		g.needs(foundVar + " TINYINT DEFAULT 0")
	} else {
		w.line("DECLARE CONTINUE HANDLER FOR NOT FOUND SET %s = 1;", done)
	}
	w.line("OPEN %s;", cursor)
	w.line("%s: LOOP", frame.out)
	w.in()
	// The flag is cleared before every fetch, so that nothing the body
	// did can end the loop.
	w.line("SET %s = 0;", done)
	w.line("FETCH %s INTO %s;", cursor, strings.Join(targets, ", "))
	w.line("IF %s = 1 THEN", done)
	w.line("  LEAVE %s;", frame.out)
	w.line("END IF;")
	g.myStmts(w, s.body, false)
	w.out()
	w.line("END LOOP;")
	w.line("CLOSE %s;", cursor)
	w.out()
	w.line("END;")
}

// continues reports whether a CONTINUE in the statements starts the
// next round of the loop with the given label: one that names it, or one
// without a label that is not inside a loop of its own.
func continues(list []stmt, label string, inner bool) bool {
	for _, s := range list {
		switch s := s.(type) {
		case *exitStmt:
			if s.next && ((s.label == "" && !inner) || (s.label != "" && strings.EqualFold(s.label, label))) {
				return true
			}
		case *ifStmt:
			for _, b := range s.branches {
				if continues(b.body, label, inner) {
					return true
				}
			}
			if continues(s.els, label, inner) {
				return true
			}
		case *caseStmt:
			for _, b := range s.branches {
				if continues(b.body, label, inner) {
					return true
				}
			}
			if continues(s.els, label, inner) {
				return true
			}
		case *blockStmt:
			if continues(s.b.body, label, inner) {
				return true
			}
		case *loopStmt:
			if continues(s.body, label, true) {
				return true
			}
		}
	}
	return false
}

func hasStar(q *sqlStmt) bool {
	for _, c := range q.columns {
		if _, ok := c.e.(*starExpr); ok {
			return true
		}
	}
	return false
}

// starOrder lists the fields of a query that selects with "*", in the
// order of the columns.
func (g *gen) starOrder(q *sqlStmt) []string {
	var out []string
	for _, c := range q.columns {
		star, ok := c.e.(*starExpr)
		if !ok {
			out = append(out, strings.ToUpper(c.name))
			continue
		}
		for _, ref := range q.tables {
			if len(star.qualifier) > 0 {
				name := star.qualifier[len(star.qualifier)-1].name
				if !strings.EqualFold(ref.alias, name) && !(len(ref.name) > 0 && strings.EqualFold(ref.name[len(ref.name)-1].name, name)) {
					continue
				}
			}
			if t := g.findTable(ref.name); t != nil && ref.sub == nil {
				for _, col := range t.Columns {
					out = append(out, strings.ToUpper(col.Name))
				}
			}
		}
	}
	return out
}

// myExit writes EXIT and CONTINUE as LEAVE and ITERATE.
func (g *gen) myExit(w *writer, s *exitStmt) {
	var frame *loopFrame
	for i := len(g.loops) - 1; i >= 0; i-- {
		f := g.loops[i]
		if (s.label == "" && !f.block) || (s.label != "" && f.label == strings.ToUpper(s.label)) {
			frame = f
			break
		}
	}
	if frame == nil {
		g.refuse(s.line, "the statement leaves a loop or block that is not around it")
		return
	}
	if frame.depth != g.handlerDepth {
		g.refuse(s.line, "the statement leaves a loop from inside an error handler, which MySQL does not allow")
		return
	}
	word := "LEAVE " + frame.out
	if s.next {
		switch {
		case frame.block:
			g.refuse(s.line, "CONTINUE names a block, not a loop")
		case frame.nextLabel != "":
			word = "LEAVE " + frame.nextLabel
		default:
			word = "ITERATE " + frame.out
		}
	}
	if s.when == nil {
		w.line("%s;", word)
		return
	}
	w.line("IF %s THEN", g.cond(s.when))
	w.line("  %s;", word)
	w.line("END IF;")
}

// myReturn writes RETURN. A function returns a value as everywhere; a
// procedure and a trigger have to leave their outermost block instead.
func (g *gen) myReturn(w *writer, s *returnStmt, last bool) {
	if g.r.kind == "function" {
		if s.value == nil {
			g.refuse(s.line, "RETURN without a value in a function")
			return
		}
		ret := tUnknown
		if m, ok := mapType(g.from, g.to, g.r.returns); ok {
			ret = m.typ
		}
		w.line("RETURN %s;", g.assigned(s.value, ret))
		return
	}
	if g.r.kind == "trigger" && g.from == Postgres {
		g.checkTriggerReturn(s)
	} else if s.value != nil {
		g.value(s.value)
	}
	switch {
	case last:
		// The end of the outermost block is the end of the routine.
	case g.handlerDepth > 0:
		g.refuse(s.line, "RETURN inside an error handler, other than as the last statement of a handler of the outermost block: MySQL cannot leave the routine from there")
	default:
		g.needBodyLabel = true
		w.line("LEAVE %s;", bodyLabel)
	}
}

// checkTriggerReturn refuses the results of a PostgreSQL trigger
// function that change what happens to the row. MySQL has no way to
// skip a row or to put the old one back.
func (g *gen) checkTriggerReturn(s *returnStmt) {
	h := g.r.trigger
	if h.timing != "BEFORE" {
		return
	}
	want := "NEW"
	if g.event == "DELETE" {
		want = "OLD"
	}
	got := ""
	switch v := s.value.(type) {
	case *nameExpr:
		if len(v.parts) == 1 {
			got = strings.ToUpper(v.parts[0].name)
		}
	case *litExpr:
		if v.kind == "null" {
			got = "NULL"
		}
	}
	switch {
	case got == want:
	case got == "NULL":
		g.refuse(s.line, "the trigger returns NULL, which makes PostgreSQL skip the row; a MySQL trigger can only let the change happen or fail")
	default:
		g.refuse(s.line, "on %s the trigger returns %s rather than %s, which changes what PostgreSQL does with the row; a MySQL trigger cannot do that", g.event, describeExpr(s.value), want)
	}
}

// signal writes SIGNAL. MySQL takes the message from a literal or a
// variable only, so anything else goes through a variable.
func (g *gen) signal(w *writer, state string, message expr, line int) {
	if message == nil {
		w.line("SIGNAL SQLSTATE '%s';", state)
		return
	}
	if text, ok := isLit(message, "string"); ok && len(text) <= 128 && text != "" {
		w.line("SIGNAL SQLSTATE '%s' SET MESSAGE_TEXT = %s;", state, g.stringLit(text))
		return
	}
	msg, _ := g.value(message)
	g.needs(messageVar + " TEXT")
	w.line("SET %s = %s;", messageVar, msg)
	w.line("SIGNAL SQLSTATE '%s' SET MESSAGE_TEXT = %s;", state, messageVar)
}

func (g *gen) myRaise(w *writer, s *raiseStmt) {
	switch {
	case s.reraise:
		if g.handlerDepth == 0 {
			g.refuse(s.line, "an error is passed on outside an error handler")
		}
		w.line("RESIGNAL;")
	case s.oraCode != nil:
		code, ok := intLit(s.oraCode)
		if !ok || code > -20000 || code < -20999 {
			g.refuse(s.line, "RAISE_APPLICATION_ERROR with an error number that is not a literal between -20999 and -20000")
			return
		}
		message := s.message
		if text, ok := isLit(message, "string"); !ok || text == "" {
			// A NULL message is no message in Oracle and an error of
			// its own in MySQL.
			message = &callExpr{name: []ident{{name: "NVL"}}, args: []expr{s.message, &litExpr{kind: "string", text: " "}}, argNames: make([]string, 2), line: s.line}
		}
		g.signal(w, fmt.Sprintf("45%03d", -code-20000), message, s.line)
	case g.from == Oracle:
		v := g.lookup(s.name)
		switch {
		case v != nil && v.kind == "exception" && v.sqlstate != "":
			g.signal(w, v.sqlstate, &litExpr{kind: "string", text: strings.ToLower(v.name), line: s.line}, s.line)
		case s.name == "NO_DATA_FOUND":
			g.signal(w, noDataState, &litExpr{kind: "string", text: "no data found", line: s.line}, s.line)
		default:
			g.refuse(s.line, "RAISE %s: MySQL cannot raise one of its own errors on request", s.name)
		}
	case s.level != "EXCEPTION":
		for _, a := range s.args {
			g.value(a)
		}
		g.note(s.line, "RAISE %s is dropped: MySQL has nowhere to send a message from a routine", s.level)
	case s.name != "":
		g.refuse(s.line, "RAISE %s: MySQL cannot raise one of its own errors on request", strings.ToLower(s.name))
	default:
		state := s.sqlstate
		if state == "" {
			state = "P0001"
		}
		if strings.HasPrefix(state, "00") || strings.HasPrefix(state, "01") || strings.HasPrefix(state, "02") {
			g.refuse(s.line, "RAISE with SQLSTATE %s, which is not an error in MySQL", state)
			return
		}
		if len(s.dropped) > 0 {
			g.note(s.line, "the %s of the error is dropped: a MySQL error has a message only", strings.Join(s.dropped, " and "))
		}
		message := s.message
		if s.hasFmt {
			message = g.formatMessage(s)
		}
		g.signal(w, state, message, s.line)
	}
}

// formatMessage turns the template of a PL/pgSQL RAISE and its values
// into one expression: the pieces of the template joined with the
// values, a NULL value written as PostgreSQL writes it.
func (g *gen) formatMessage(s *raiseStmt) expr {
	var parts []expr
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parts = append(parts, &litExpr{kind: "string", text: text.String(), line: s.line})
			text.Reset()
		}
	}
	next := 0
	for i := 0; i < len(s.format); i++ {
		c := s.format[i]
		if c != '%' {
			text.WriteByte(c)
			continue
		}
		if i+1 < len(s.format) && s.format[i+1] == '%' {
			text.WriteByte('%')
			i++
			continue
		}
		if next >= len(s.args) {
			g.refuse(s.line, "RAISE has more placeholders than values")
			break
		}
		flush()
		parts = append(parts, &callExpr{name: []ident{{name: "COALESCE"}}, argNames: make([]string, 2), line: s.line,
			args: []expr{&castExpr{x: s.args[next], typ: &dataType{name: "TEXT", line: s.line}, line: s.line}, &litExpr{kind: "string", text: "<NULL>", line: s.line}}})
		next++
	}
	flush()
	if len(parts) == 0 {
		return &litExpr{kind: "string", text: " ", line: s.line}
	}
	e := parts[0]
	for _, p := range parts[1:] {
		e = &binExpr{op: "||", l: e, r: p, line: s.line}
	}
	return e
}

// mySQL writes an SQL statement of the routine.
func (g *gen) mySQL(w *writer, s *sqlExec) {
	q := s.sql
	if q.verb != "SELECT" {
		if q.returning {
			g.refuse(s.line, "%s ... RETURNING: MySQL cannot hand back the rows a statement changed", q.verb)
			return
		}
		if h := g.r.trigger; h != nil && len(q.tables) > 0 && q.tables[0].sub == nil && g.tableName(q.tables[0].name) == g.tableName(h.table) {
			g.refuse(s.line, "the trigger changes the table it belongs to, which MySQL does not allow")
		}
		text, _ := g.statement(q, "")
		w.line("%s;", text)
		g.afterChange(w, s)
		return
	}
	if len(q.into) == 0 {
		g.refuse(s.line, "a SELECT that puts its result nowhere")
		return
	}
	var targets []string
	for _, t := range q.into {
		targets = append(targets, g.target(t))
	}
	into := "INTO " + strings.Join(targets, ", ")
	strict := g.from == Oracle || q.strict
	wantFound := g.foundAfter["select"] || g.foundAfter["all"]
	found := func() {
		if wantFound {
			g.needs(foundVar + " TINYINT DEFAULT 0")
			w.line("SET %s = 1;", foundVar)
		}
	}
	rowCount := func() {
		if g.countAfter["select"] || g.countAfter["all"] {
			g.needs(rowCountVar + " BIGINT")
			w.line("SET %s = 1;", rowCountVar)
		}
	}
	switch {
	case singleRow(q):
		found()
		text, _ := g.statement(q, into)
		w.line("%s;", text)
		rowCount()
	case strict:
		// MySQL only warns when the query finds nothing; the source
		// raises an error, and so does this handler.
		if g.from == Oracle {
			g.silentNoData(s)
		}
		text, _ := g.statement(q, into)
		w.line("BEGIN")
		w.line("  DECLARE EXIT HANDLER FOR NOT FOUND SIGNAL SQLSTATE '%s' SET MESSAGE_TEXT = 'query returned no rows';", noDataState)
		w.line("  %s;", text)
		w.line("END;")
		rowCount()
	default:
		// Without STRICT, PL/pgSQL takes the first row of several and
		// sets the variables to NULL when there is none.
		text, _ := g.statement(q, into)
		switch {
		case q.plain:
			text += " LIMIT 1"
		case !limitsToOne(q):
			g.note(s.line, "if the query gives more than one row PL/pgSQL takes the first; MySQL raises an error")
		}
		reset := make([]string, len(targets))
		for i, t := range targets {
			reset[i] = t + " = NULL"
		}
		if wantFound {
			reset = append(reset, foundVar+" = 0")
			g.needs(foundVar + " TINYINT DEFAULT 0")
		}
		w.line("BEGIN")
		w.line("  DECLARE CONTINUE HANDLER FOR NOT FOUND SET %s;", strings.Join(reset, ", "))
		w.in()
		found()
		w.out()
		w.line("  %s;", text)
		w.line("END;")
	}
}

// limitsToOne reports whether a query ends in LIMIT 1.
func limitsToOne(q *sqlStmt) bool {
	for _, p := range q.parts {
		if p.limit != nil {
			n, ok := intLit(p.limit.count)
			return ok && n == 1
		}
	}
	return false
}

// afterChange records how many rows a statement changed, for the code
// that asks afterwards.
func (g *gen) afterChange(w *writer, s *sqlExec) {
	verb := strings.ToLower(s.sql.verb)
	found := g.foundAfter[verb] || g.foundAfter["all"]
	if !found && !g.countAfter[verb] && !g.countAfter["all"] {
		return
	}
	if s.sql.verb == "UPDATE" {
		g.note(s.line, "the number of rows the UPDATE touched is read afterwards; MySQL counts the rows whose values changed, %s the rows that matched", g.from)
	}
	g.needs(rowCountVar + " BIGINT")
	w.line("SET %s = ROW_COUNT();", rowCountVar)
	if found {
		g.needs(foundVar + " TINYINT DEFAULT 0")
		w.line("SET %s = (%s > 0);", foundVar, rowCountVar)
	}
}

// Triggers.

func (g *gen) myTrigger() []string {
	h := g.r.trigger
	switch {
	case h.timing == "INSTEAD OF":
		g.refuse(h.line, "an INSTEAD OF trigger: MySQL has no triggers on views")
		return nil
	case !h.forEachRow:
		g.refuse(h.line, "a statement-level trigger: MySQL only has triggers that fire for each row")
		return nil
	case len(h.updateOf) > 0:
		g.refuse(h.line, "a trigger on UPDATE OF certain columns: MySQL fires a trigger for every UPDATE")
		return nil
	}
	name := g.fold(h.name[len(h.name)-1])
	suffix := map[string]string{"INSERT": "_ins", "UPDATE": "_upd", "DELETE": "_del"}
	var out []string
	for _, event := range h.events {
		sub := newGen(g.from, g.to, g.obj, g.opt, g.r)
		sub.notes, sub.refusals, sub.seen = g.notes, g.refusals, g.seen
		sub.event = event
		sub.usesRowCount, sub.usesFound, sub.cursors = g.usesRowCount, g.usesFound, g.cursors
		sub.push()
		b := g.r.body
		if h.when != nil {
			// A MySQL trigger has no WHEN clause; the body tests the
			// condition itself.
			b = &block{label: b.label, decls: b.decls, handlers: b.handlers, line: b.line,
				body: []stmt{&ifStmt{branches: []ifBranch{{cond: h.when, body: b.body}}, line: h.line}}}
			sub.inWhen = true
		}
		body := sub.myBody(b, nil)
		g.notes, g.refusals = sub.notes, sub.refusals
		n := name
		if len(h.events) > 1 {
			n += suffix[event]
		}
		if len([]rune(n)) > 64 {
			g.refuse(h.line, "the trigger's name is longer than the 64 characters MySQL allows")
		}
		out = append(out, "CREATE TRIGGER "+g.quote(n)+" "+h.timing+" "+event+" ON "+g.tableName(h.table)+"\nFOR EACH ROW\n"+body)
	}
	return out
}
