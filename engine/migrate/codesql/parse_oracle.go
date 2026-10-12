package codesql

import (
	"strconv"
	"strings"
)

// oracleParser reads PL/SQL: a standalone function, procedure or trigger
// as ALL_SOURCE or DBMS_METADATA gives it, with or without the leading
// CREATE OR REPLACE.
type oracleParser struct{ *parser }

func parseOracle(src string) (r *routine, err error) {
	toks, err := lex(Oracle, src)
	if err != nil {
		return nil, err
	}
	p := &oracleParser{&parser{d: Oracle, toks: toks}}
	defer catch(&err)
	return p.routine(), nil
}

// catch turns the panic of a parser that gave up into the error it
// carries.
func catch(err *error) {
	switch e := recover().(type) {
	case nil:
	case *parseError:
		*err = e
	case *unsupportedError:
		*err = e
	default:
		panic(e)
	}
}

func (p *oracleParser) routine() *routine {
	if p.acceptWord("CREATE") {
		p.acceptWords("OR", "REPLACE")
		p.acceptWord("EDITIONABLE", "NONEDITIONABLE")
	}
	t := p.next()
	r := &routine{}
	switch t.up {
	case "FUNCTION", "PROCEDURE":
		r.kind = strings.ToLower(t.up)
		r.name = p.qualified()
		r.params = p.params()
		if r.kind == "function" {
			p.expectWord("RETURN")
			r.returns = p.parseType()
		}
		p.attributes(r)
		if !p.acceptWord("IS", "AS") {
			p.fail("expected IS or AS, found %s", p.peek().describe())
		}
		if p.isWord("LANGUAGE", "EXTERNAL") {
			p.refuse(p.peek().line, "a routine written in another language (%s)", p.peek().up)
		}
		r.body = &block{line: p.peek().line}
		r.body.decls = p.decls()
		p.blockBody(r.body)
	case "TRIGGER":
		r.kind = "trigger"
		r.trigger = p.triggerHead()
		r.name = r.trigger.name
		r.body = &block{line: p.peek().line}
		if p.isWord("CALL") {
			p.refuse(p.peek().line, "a trigger that only calls a procedure (CALL)")
		}
		if p.isWord("COMPOUND") {
			p.refuse(p.peek().line, "a compound trigger")
		}
		if p.acceptWord("DECLARE") {
			r.body.decls = p.decls()
		}
		p.blockBody(r.body)
	case "PACKAGE":
		p.refuse(t.line, "a package: its routines share state and are called through its name, which neither target database has")
	case "TYPE":
		p.refuse(t.line, "an object type")
	default:
		p.pos--
		p.fail("expected FUNCTION, PROCEDURE or TRIGGER, found %s", t.describe())
	}
	p.trailer()
	return r
}

// trailer checks what follows the routine. SQL*Plus scripts end it with
// a slash, and DBMS_METADATA adds a statement that enables the trigger.
func (p *oracleParser) trailer() {
	for p.pos < len(p.toks) {
		t := p.peek()
		switch {
		case t.op("/"), t.op(";"):
			p.pos++
		case t.is("ALTER") && p.peekAt(1).is("TRIGGER"):
			for p.pos < len(p.toks) && !p.isOp(";") && !p.isOp("/") {
				if p.isWord("DISABLE") {
					p.refuse(t.line, "a trigger that is disabled on the source")
				}
				p.pos++
			}
		default:
			p.fail("unexpected %s after the end of the routine", t.describe())
		}
	}
}

func (p *oracleParser) params() []*param {
	if !p.acceptOp("(") {
		return nil
	}
	var out []*param
	for {
		t := p.peek()
		pm := &param{name: p.ident().name, mode: "IN", line: t.line}
		in := p.acceptWord("IN")
		if p.acceptWord("OUT") {
			pm.mode = "OUT"
			if in {
				pm.mode = "INOUT"
			}
		}
		p.acceptWord("NOCOPY")
		pm.typ = p.parseType()
		if p.acceptOp(":=") || p.acceptWord("DEFAULT") {
			pm.def = p.expr()
		}
		out = append(out, pm)
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(")")
	return out
}

// attributes reads what a function or procedure says about itself
// between its signature and its body.
func (p *oracleParser) attributes(r *routine) {
	for {
		t := p.peek()
		switch {
		case p.acceptWord("DETERMINISTIC"):
			r.deterministic = true
		case p.acceptWord("AUTHID"):
			p.pos++
		case p.acceptWord("RESULT_CACHE", "PARALLEL_ENABLE"):
			if p.acceptWord("RELIES_ON") || p.isOp("(") {
				p.skipParens()
			}
		case p.acceptWord("ACCESSIBLE"):
			p.expectWord("BY")
			p.skipParens()
		case p.acceptWord("SHARING"):
			p.expectOp("=")
			p.pos++
		case p.isWord("PIPELINED"):
			p.refuse(t.line, "a pipelined function, which returns rows one at a time")
		case p.isWord("AGGREGATE"):
			p.refuse(t.line, "a user-defined aggregate function")
		case p.isWord("SQL_MACRO"):
			p.refuse(t.line, "an SQL macro")
		default:
			return
		}
	}
}

var dmlEvents = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true}

func (p *oracleParser) triggerHead() *triggerHead {
	h := &triggerHead{line: p.peek().line, newName: "NEW", oldName: "OLD"}
	h.name = p.qualified()
	t := p.next()
	switch {
	case t.up == "BEFORE" || t.up == "AFTER":
		h.timing = t.up
	case t.up == "INSTEAD":
		p.expectWord("OF")
		h.timing = "INSTEAD OF"
	case t.up == "FOR":
		p.refuse(t.line, "a compound trigger")
	default:
		p.pos--
		p.fail("expected BEFORE, AFTER or INSTEAD OF, found %s", t.describe())
	}
	for {
		ev := p.next()
		if !dmlEvents[ev.up] {
			p.refuse(ev.line, "a trigger on %s: only triggers on INSERT, UPDATE and DELETE of a table are converted", ev.up)
		}
		h.events = append(h.events, ev.up)
		if ev.up == "UPDATE" && p.acceptWord("OF") {
			for {
				h.updateOf = append(h.updateOf, p.ident())
				if !p.acceptOp(",") {
					break
				}
			}
		}
		if !p.acceptWord("OR") {
			break
		}
	}
	p.expectWord("ON")
	if p.isWord("NESTED") || ((p.isWord("DATABASE") || p.isWord("SCHEMA")) && !p.peekAt(1).op(".")) && p.peekAt(1).kind == tWord {
		p.refuse(p.peek().line, "a trigger on %s", p.peek().up)
	}
	h.table = p.qualified()
	for {
		switch {
		case p.acceptWord("REFERENCING"):
			for p.isWord("OLD", "NEW", "PARENT") {
				which := p.next().up
				p.acceptWord("AS")
				name := strings.ToUpper(p.ident().name)
				switch which {
				case "OLD":
					h.oldName = name
				case "NEW":
					h.newName = name
				}
			}
		case p.acceptWords("FOR", "EACH", "ROW"):
			h.forEachRow = true
		case p.isWord("FOLLOWS", "PRECEDES"):
			p.refuse(p.peek().line, "a trigger with a fixed place in the firing order (%s)", p.peek().up)
		case p.acceptWord("ENABLE"):
		case p.isWord("DISABLE"):
			p.refuse(p.peek().line, "a trigger that is disabled on the source")
		case p.acceptWord("EDITIONABLE", "NONEDITIONABLE", "FORWARD", "REVERSE", "CROSSEDITION"):
		case p.acceptWord("WHEN"):
			p.expectOp("(")
			h.when = p.expr()
			p.expectOp(")")
		default:
			return h
		}
	}
}

// decls reads the declarations in front of BEGIN.
func (p *oracleParser) decls() []decl {
	var out []decl
	for !p.isWord("BEGIN") {
		t := p.peek()
		switch {
		case t.kind == tEOF:
			p.fail("expected BEGIN, found the end of the text")
		case p.acceptWord("PRAGMA"):
			name := p.next().up
			switch name {
			case "AUTONOMOUS_TRANSACTION":
				p.refuse(t.line, "an autonomous transaction (PRAGMA AUTONOMOUS_TRANSACTION)")
			case "EXCEPTION_INIT":
				p.expectOp("(")
				exc := strings.ToUpper(p.ident().name)
				p.expectOp(",")
				neg := p.acceptOp("-")
				num := p.next()
				code, err := strconv.Atoi(num.text)
				if num.kind != tNumber || err != nil {
					p.fail("the error number of EXCEPTION_INIT is not a number")
				}
				if neg {
					code = -code
				}
				p.expectOp(")")
				found := false
				for _, d := range out {
					if e, ok := d.(*exceptionDecl); ok && strings.ToUpper(e.name) == exc {
						e.code, found = code, true
					}
				}
				if !found {
					p.fail("EXCEPTION_INIT names %s, which is not declared before it", exc)
				}
			case "UDF", "INLINE":
				if p.isOp("(") {
					p.skipParens()
				}
			default:
				p.refuse(t.line, "PRAGMA %s", name)
			}
			p.expectOp(";")
		case p.acceptWord("CURSOR"):
			c := &cursorDecl{name: p.ident().name, line: t.line}
			c.params = p.params()
			if p.acceptWord("RETURN") {
				p.parseType()
			}
			p.expectWord("IS")
			c.query = p.query()
			p.expectOp(";")
			out = append(out, c)
		case t.is("TYPE") || t.is("SUBTYPE"):
			p.pos++
			name := p.ident().name
			what := "the type " + name
			if p.acceptWord("IS") {
				switch {
				case p.isWord("RECORD"):
					what = "the record type " + name
				case p.isWord("TABLE"), p.isWord("VARRAY"), p.isWord("VARYING"):
					what = "the collection type " + name
				case p.isWord("REF"):
					what = "the cursor variable type " + name
				}
			}
			p.skipStatement()
			out = append(out, &unsupportedDecl{what: what, line: t.line})
		case t.is("PROCEDURE") || t.is("FUNCTION"):
			p.refuse(t.line, "a %s declared inside the routine", strings.ToLower(t.up))
		case p.peekAt(1).is("EXCEPTION"):
			name := p.ident().name
			p.pos++
			p.expectOp(";")
			out = append(out, &exceptionDecl{name: name, line: t.line})
		default:
			v := &varDecl{name: p.ident().name, line: t.line}
			v.constant = p.acceptWord("CONSTANT")
			v.typ = p.parseType()
			if p.acceptWords("NOT", "NULL") {
				v.notNull = true
			}
			if p.acceptOp(":=") || p.acceptWord("DEFAULT") {
				v.def = p.expr()
			}
			p.expectOp(";")
			out = append(out, v)
		}
	}
	return out
}

// skipStatement moves past everything up to and including the next
// semicolon outside parentheses.
func (p *parser) skipStatement() {
	depth := 0
	for p.pos < len(p.toks) {
		t := p.next()
		switch {
		case t.op("("):
			depth++
		case t.op(")"):
			depth--
		case t.op(";") && depth <= 0:
			return
		}
	}
}

// blockBody reads BEGIN ... [EXCEPTION ...] END [name]; into b.
func (p *oracleParser) blockBody(b *block) {
	p.expectWord("BEGIN")
	b.body = p.stmts()
	if p.acceptWord("EXCEPTION") {
		for p.isWord("WHEN") {
			h := &handler{line: p.next().line, action: "EXIT"}
			for {
				name := p.qualified()
				if len(name) > 1 {
					p.refuse(h.line, "an exception of a package (%s.%s)", name[0].name, name[1].name)
				}
				if up := strings.ToUpper(name[0].name); up == "OTHERS" {
					h.conds = append(h.conds, condition{kind: "others"})
				} else {
					h.conds = append(h.conds, condition{kind: "name", value: up})
				}
				if !p.acceptWord("OR") {
					break
				}
			}
			p.expectWord("THEN")
			h.body = p.stmts()
			b.handlers = append(b.handlers, h)
		}
	}
	p.expectWord("END")
	if !p.isOp(";") {
		p.ident()
	}
	p.expectOp(";")
}

// stmts reads statements up to the word that closes the list.
func (p *oracleParser) stmts() []stmt {
	var out []stmt
	for !p.isWord("END", "EXCEPTION", "ELSIF", "ELSE", "WHEN") {
		if p.peek().kind == tEOF {
			p.fail("expected END, found the end of the text")
		}
		out = append(out, p.statement())
	}
	return out
}

func (p *oracleParser) statement() stmt {
	label := ""
	if p.acceptOp("<<") {
		label = p.ident().name
		p.expectOp(">>")
	}
	t := p.peek()
	switch t.up {
	case "DECLARE", "BEGIN":
		b := &block{label: label, line: t.line}
		if p.acceptWord("DECLARE") {
			b.decls = p.decls()
		}
		p.blockBody(b)
		return &blockStmt{b}
	case "IF":
		p.pos++
		s := &ifStmt{line: t.line}
		for {
			br := ifBranch{cond: p.expr()}
			p.expectWord("THEN")
			br.body = p.stmts()
			s.branches = append(s.branches, br)
			if !p.acceptWord("ELSIF") {
				break
			}
		}
		if p.acceptWord("ELSE") {
			s.hasElse = true
			s.els = p.stmts()
		}
		p.expectWord("END")
		p.expectWord("IF")
		p.expectOp(";")
		return s
	case "CASE":
		p.pos++
		s := &caseStmt{line: t.line}
		if !p.isWord("WHEN") {
			s.operand = p.expr()
		}
		for p.acceptWord("WHEN") {
			br := caseBranch{values: []expr{p.expr()}}
			p.expectWord("THEN")
			br.body = p.stmts()
			s.branches = append(s.branches, br)
		}
		if p.acceptWord("ELSE") {
			s.hasElse = true
			s.els = p.stmts()
		}
		p.expectWord("END")
		p.expectWord("CASE")
		if !p.isOp(";") {
			p.ident()
		}
		p.expectOp(";")
		return s
	case "LOOP", "WHILE", "FOR":
		return p.loop(label)
	case "EXIT", "CONTINUE":
		if p.peekAt(1).op(":=") || p.peekAt(1).op("(") {
			break
		}
		p.pos++
		s := &exitStmt{next: t.up == "CONTINUE", line: t.line}
		if p.isName() && !p.isWord("WHEN") {
			s.label = p.ident().name
		}
		if p.acceptWord("WHEN") {
			s.when = p.expr()
		}
		p.expectOp(";")
		return s
	case "GOTO":
		p.refuse(t.line, "GOTO")
	case "NULL":
		p.pos++
		p.expectOp(";")
		return &nullStmt{line: t.line}
	case "RETURN":
		p.pos++
		s := &returnStmt{line: t.line}
		if !p.isOp(";") {
			s.value = p.expr()
		}
		p.expectOp(";")
		return s
	case "RAISE":
		p.pos++
		s := &raiseStmt{line: t.line, level: "EXCEPTION"}
		if p.isOp(";") {
			s.reraise = true
		} else {
			name := p.qualified()
			if len(name) > 1 {
				p.refuse(t.line, "an exception of a package (%s.%s)", name[0].name, name[1].name)
			}
			s.name = strings.ToUpper(name[0].name)
		}
		p.expectOp(";")
		return s
	case "OPEN":
		p.pos++
		s := &openStmt{cursor: p.ident().name, line: t.line}
		if p.isWord("FOR") {
			p.refuse(t.line, "a cursor variable (OPEN ... FOR)")
		}
		if p.acceptOp("(") {
			for {
				s.args = append(s.args, p.expr())
				if !p.acceptOp(",") {
					break
				}
			}
			p.expectOp(")")
		}
		p.expectOp(";")
		return s
	case "FETCH":
		p.pos++
		s := &fetchStmt{cursor: p.ident().name, line: t.line}
		if p.isWord("BULK") {
			p.refuse(t.line, "BULK COLLECT")
		}
		p.expectWord("INTO")
		for {
			s.into = append(s.into, p.postfix())
			if !p.acceptOp(",") {
				break
			}
		}
		if p.isWord("LIMIT") {
			p.refuse(t.line, "BULK COLLECT")
		}
		p.expectOp(";")
		return s
	case "CLOSE":
		p.pos++
		s := &closeStmt{cursor: p.ident().name, line: t.line}
		p.expectOp(";")
		return s
	case "EXECUTE":
		if p.peekAt(1).is("IMMEDIATE") {
			p.refuse(t.line, "dynamic SQL (EXECUTE IMMEDIATE): the statement is built while the routine runs and cannot be translated ahead of time")
		}
	case "FORALL":
		p.refuse(t.line, "FORALL")
	case "PIPE":
		p.refuse(t.line, "PIPE ROW")
	case "COMMIT", "ROLLBACK":
		p.pos++
		p.acceptWord("WORK")
		if p.isWord("TO") {
			p.refuse(t.line, "ROLLBACK TO a savepoint")
		}
		p.expectOp(";")
		return &txStmt{verb: t.up, line: t.line}
	case "SAVEPOINT":
		p.refuse(t.line, "a savepoint")
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE":
		if p.peekAt(1).op(":=") {
			break
		}
		s := &sqlExec{sql: p.sqlStatement(), line: t.line}
		p.expectOp(";")
		return s
	case "MERGE":
		p.refuse(t.line, "MERGE")
	case "LOCK":
		if p.peekAt(1).is("TABLE") {
			p.refuse(t.line, "LOCK TABLE")
		}
	case "SET":
		if p.peekAt(1).is("TRANSACTION") {
			p.refuse(t.line, "SET TRANSACTION")
		}
	}
	if t.kind != tWord && t.kind != tQuoted && t.kind != tBind {
		p.fail("expected a statement, found %s", t.describe())
	}
	// What is left starts with a name: an assignment or a call.
	target := p.primary()
	if p.acceptOp(":=") {
		s := &assignStmt{target: target, value: p.expr(), line: t.line}
		p.expectOp(";")
		return s
	}
	p.expectOp(";")
	switch c := target.(type) {
	case *nameExpr:
		if c.bind {
			p.fail("a statement consists of the row name %s alone", c.parts[0].name)
		}
		return p.call(c.parts, nil, nil, t.line)
	case *callExpr:
		return p.call(c.name, c.args, c.argNames, t.line)
	}
	p.fail("expected a statement, found %s", t.describe())
	return nil
}

// call sorts a procedure call into the built-ins that have a statement
// of their own and ordinary calls.
func (p *oracleParser) call(name []ident, args []expr, argNames []string, line int) stmt {
	var parts []string
	for _, n := range name {
		parts = append(parts, strings.ToUpper(n.name))
	}
	switch full := strings.Join(parts, "."); full {
	case "RAISE_APPLICATION_ERROR":
		if len(args) < 2 {
			p.fail("RAISE_APPLICATION_ERROR needs an error number and a message")
		}
		return &raiseStmt{oraCode: args[0], message: args[1], level: "EXCEPTION", line: line}
	case "DBMS_OUTPUT.PUT_LINE":
		if len(args) != 1 {
			p.fail("DBMS_OUTPUT.PUT_LINE takes one argument")
		}
		return &outputStmt{value: args[0], line: line}
	}
	return &callStmt{name: name, args: args, argNames: argNames, line: line}
}

func (p *oracleParser) loop(label string) stmt {
	t := p.next()
	s := &loopStmt{label: label, line: t.line}
	switch t.up {
	case "LOOP":
		s.kind = "loop"
		p.pos--
	case "WHILE":
		s.kind = "while"
		s.cond = p.expr()
	case "FOR":
		s.variable = p.ident().name
		p.expectWord("IN")
		s.reverse = p.acceptWord("REVERSE")
		if p.isOp("(") && (p.peekAt(1).is("SELECT") || p.peekAt(1).is("WITH")) {
			p.pos++
			s.kind = "query"
			s.query = p.query()
			p.expectOp(")")
			break
		}
		first := p.concat()
		if p.acceptOp("..") {
			s.kind = "range"
			s.lo, s.hi = first, p.concat()
			break
		}
		s.kind = "cursor"
		switch c := first.(type) {
		case *nameExpr:
			s.cursor = c.parts[0].name
		case *callExpr:
			s.cursor, s.args = c.name[0].name, c.args
		default:
			p.fail("expected a range, a query or a cursor after IN")
		}
	}
	p.expectWord("LOOP")
	s.body = p.stmts()
	p.expectWord("END")
	p.expectWord("LOOP")
	if !p.isOp(";") {
		p.ident()
	}
	p.expectOp(";")
	return s
}
