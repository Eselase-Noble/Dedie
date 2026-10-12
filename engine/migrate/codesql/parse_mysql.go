package codesql

import (
	"strings"
)

// mysqlParser reads a stored function, procedure or trigger as SHOW
// CREATE prints it.
type mysqlParser struct{ *parser }

func parseMySQL(src string) (r *routine, err error) {
	toks, err := lex(MySQL, src)
	if err != nil {
		return nil, err
	}
	p := &mysqlParser{&parser{d: MySQL, toks: toks}}
	defer catch(&err)
	return p.routine(), nil
}

func (p *mysqlParser) routine() *routine {
	p.expectWord("CREATE")
	// CREATE DEFINER=`user`@`host` ..., and MariaDB's OR REPLACE.
	for !p.isWord("FUNCTION", "PROCEDURE", "TRIGGER") {
		if p.peek().kind == tEOF {
			p.fail("expected FUNCTION, PROCEDURE or TRIGGER")
		}
		if p.isWord("AGGREGATE") {
			p.refuse(p.peek().line, "a user-defined aggregate function")
		}
		p.pos++
	}
	t := p.next()
	r := &routine{kind: strings.ToLower(t.up)}
	p.acceptWords("IF", "NOT", "EXISTS")
	switch r.kind {
	case "trigger":
		r.trigger = p.triggerHead()
		r.name = r.trigger.name
	default:
		r.name = p.qualified()
		r.params = p.params(r.kind == "procedure")
		if r.kind == "function" {
			p.expectWord("RETURNS")
			r.returns = p.parseType()
		}
		p.characteristics(r)
	}
	body := p.statement()
	if b, ok := body.(*blockStmt); ok {
		r.body = b.b
	} else {
		r.body = &block{body: []stmt{body}, line: body.stmtLine()}
	}
	p.acceptOp(";")
	if p.pos < len(p.toks) {
		p.fail("unexpected %s after the end of the routine", p.peek().describe())
	}
	return r
}

func (p *mysqlParser) params(modes bool) []*param {
	p.expectOp("(")
	var out []*param
	for !p.isOp(")") {
		t := p.peek()
		pm := &param{mode: "IN", line: t.line}
		if modes && p.isWord("IN", "OUT", "INOUT") && (p.peekAt(1).kind == tWord || p.peekAt(1).kind == tQuoted) && !p.peekAt(2).op(",") && !p.peekAt(2).op(")") {
			pm.mode = p.next().up
		}
		pm.name = p.ident().name
		pm.typ = p.parseType()
		out = append(out, pm)
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(")")
	return out
}

// characteristics reads what a routine says about itself between its
// signature and its body.
func (p *mysqlParser) characteristics(r *routine) {
	for {
		switch {
		case p.acceptWord("COMMENT"):
			p.pos++
		case p.acceptWords("LANGUAGE", "SQL"):
		case p.acceptWords("NOT", "DETERMINISTIC"):
		case p.acceptWord("DETERMINISTIC"):
			r.deterministic = true
		case p.acceptWords("CONTAINS", "SQL"), p.acceptWords("NO", "SQL"), p.acceptWords("READS", "SQL", "DATA"), p.acceptWords("MODIFIES", "SQL", "DATA"):
		case p.acceptWords("SQL", "SECURITY"):
			p.pos++
		default:
			return
		}
	}
}

func (p *mysqlParser) triggerHead() *triggerHead {
	h := &triggerHead{line: p.peek().line, newName: "NEW", oldName: "OLD", forEachRow: true}
	h.name = p.qualified()
	t := p.next()
	if t.up != "BEFORE" && t.up != "AFTER" {
		p.pos--
		p.fail("expected BEFORE or AFTER, found %s", t.describe())
	}
	h.timing = t.up
	ev := p.next()
	if !dmlEvents[ev.up] {
		p.pos--
		p.fail("expected INSERT, UPDATE or DELETE, found %s", ev.describe())
	}
	h.events = []string{ev.up}
	p.expectWord("ON")
	h.table = p.qualified()
	p.expectWord("FOR")
	p.expectWord("EACH")
	p.expectWord("ROW")
	if p.isWord("FOLLOWS", "PRECEDES") {
		h.order = p.next().up + " " + p.ident().name
	}
	return h
}

// stmts reads statements, each ended by a semicolon, up to the word
// that closes the list.
func (p *mysqlParser) stmts() []stmt {
	var out []stmt
	for !p.isWord("END", "ELSEIF", "ELSE", "WHEN", "UNTIL") {
		if p.peek().kind == tEOF {
			p.fail("expected END, found the end of the text")
		}
		s := p.statement()
		if b, ok := s.(*blockStmt); ok && b.b.plain {
			out = append(out, b.b.body...)
		} else {
			out = append(out, s)
		}
		p.expectOp(";")
	}
	return out
}

// statement reads one statement without the semicolon after it.
func (p *mysqlParser) statement() stmt {
	label := ""
	if p.peek().kind == tWord && p.peekAt(1).op(":") && !p.isWord("ELSE") {
		label = p.next().text
		p.pos++
	}
	t := p.peek()
	closing := func(word string) {
		p.expectWord("END")
		p.expectWord(word)
		if label != "" && p.isName() && strings.EqualFold(p.peek().text, label) {
			p.pos++
		}
	}
	switch t.up {
	case "BEGIN":
		if p.peekAt(1).is("WORK") {
			p.refuse(t.line, "BEGIN WORK")
		}
		p.pos++
		p.acceptWords("NOT", "ATOMIC")
		b := &block{label: label, line: t.line}
		p.decls(b)
		b.body = p.stmts()
		p.expectWord("END")
		if label != "" && p.isName() && strings.EqualFold(p.peek().text, label) {
			p.pos++
		}
		return &blockStmt{b}
	case "IF":
		if p.peekAt(1).op("(") && p.ifIsFunction() {
			break
		}
		p.pos++
		s := &ifStmt{line: t.line}
		for {
			br := ifBranch{cond: p.expr()}
			p.expectWord("THEN")
			br.body = p.stmts()
			s.branches = append(s.branches, br)
			if !p.acceptWord("ELSEIF") {
				break
			}
		}
		if p.acceptWord("ELSE") {
			s.hasElse = true
			s.els = p.stmts()
		}
		p.expectWord("END")
		p.expectWord("IF")
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
		return s
	case "LOOP":
		p.pos++
		s := &loopStmt{kind: "loop", label: label, line: t.line, body: p.stmts()}
		closing("LOOP")
		return s
	case "WHILE":
		p.pos++
		s := &loopStmt{kind: "while", label: label, line: t.line, cond: p.expr()}
		p.expectWord("DO")
		s.body = p.stmts()
		closing("WHILE")
		return s
	case "REPEAT":
		if p.peekAt(1).op("(") {
			break
		}
		p.pos++
		s := &loopStmt{kind: "repeat", label: label, line: t.line, body: p.stmts()}
		p.expectWord("UNTIL")
		s.cond = p.expr()
		closing("REPEAT")
		return s
	case "LEAVE", "ITERATE":
		p.pos++
		return &exitStmt{next: t.up == "ITERATE", label: p.ident().name, line: t.line}
	case "RETURN":
		p.pos++
		return &returnStmt{value: p.expr(), line: t.line}
	case "SET":
		return p.set()
	case "OPEN":
		p.pos++
		return &openStmt{cursor: p.ident().name, line: t.line}
	case "CLOSE":
		p.pos++
		return &closeStmt{cursor: p.ident().name, line: t.line}
	case "FETCH":
		p.pos++
		p.acceptWord("NEXT")
		p.acceptWord("FROM")
		s := &fetchStmt{cursor: p.ident().name, line: t.line}
		p.expectWord("INTO")
		for {
			s.into = append(s.into, p.postfix())
			if !p.acceptOp(",") {
				break
			}
		}
		return s
	case "SIGNAL", "RESIGNAL":
		return p.signal()
	case "CALL":
		p.pos++
		s := &callStmt{name: p.qualified(), line: t.line}
		if p.acceptOp("(") {
			for !p.isOp(")") {
				s.args = append(s.args, p.expr())
				if !p.acceptOp(",") {
					break
				}
			}
			p.expectOp(")")
		}
		return s
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE":
		return &sqlExec{sql: p.sqlStatement(), line: t.line}
	case "START":
		p.pos++
		p.expectWord("TRANSACTION")
		if !p.isOp(";") {
			p.refuse(t.line, "START TRANSACTION with options")
		}
		return &txStmt{verb: "START", line: t.line}
	case "COMMIT", "ROLLBACK":
		p.pos++
		p.acceptWord("WORK")
		if p.isWord("TO") {
			p.refuse(t.line, "ROLLBACK TO a savepoint")
		}
		if !p.isOp(";") && !p.isWord("END") {
			p.refuse(t.line, "%s with options", t.up)
		}
		return &txStmt{verb: t.up, line: t.line}
	case "DECLARE":
		p.fail("DECLARE is only allowed at the start of a block")
	case "PREPARE", "EXECUTE", "DEALLOCATE":
		p.refuse(t.line, "dynamic SQL (%s): the statement is built while the routine runs and cannot be translated ahead of time", t.up)
	case "REPLACE":
		p.refuse(t.line, "REPLACE INTO")
	case "GET":
		p.refuse(t.line, "GET DIAGNOSTICS")
	case "SAVEPOINT", "RELEASE":
		p.refuse(t.line, "a savepoint")
	case "TRUNCATE", "CREATE", "DROP", "ALTER", "LOCK", "UNLOCK", "DO", "LOAD", "ANALYZE", "OPTIMIZE", "RENAME", "GRANT", "REVOKE", "FLUSH", "KILL", "USE", "SHOW", "HANDLER":
		p.refuse(t.line, "the statement %s", t.up)
	}
	p.fail("expected a statement, found %s", t.describe())
	return nil
}

// ifIsFunction tells the function IF(a, b, c) from the statement
// "IF (a) THEN": the function's parentheses hold commas.
func (p *mysqlParser) ifIsFunction() bool {
	depth := 0
	for i := 1; ; i++ {
		t := p.peekAt(i)
		switch {
		case t.kind == tEOF:
			return false
		case t.op("("):
			depth++
		case t.op(")"):
			if depth--; depth == 0 {
				return false
			}
		case t.op(",") && depth == 1:
			return true
		}
	}
}

// set reads SET with its list of assignments. More than one become a
// run of assignments in a block of their own.
func (p *mysqlParser) set() stmt {
	t := p.next()
	var out []stmt
	for {
		tk := p.peek()
		switch {
		case tk.kind == tUserVar:
			p.refuse(tk.line, "the session variable @%s", tk.text)
		case tk.op("@"), tk.is("GLOBAL"), tk.is("SESSION"), tk.is("NAMES"), tk.is("PERSIST"), tk.is("TRANSACTION"), tk.is("AUTOCOMMIT"):
			p.refuse(tk.line, "SET of a server setting")
		}
		target := &nameExpr{parts: p.qualified(), line: tk.line}
		if !p.acceptOp("=") && !p.acceptOp(":=") {
			p.fail("expected = after %s", describeExpr(target))
		}
		out = append(out, &assignStmt{target: target, value: p.expr(), line: tk.line})
		if !p.acceptOp(",") {
			break
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return &blockStmt{&block{body: out, line: t.line, plain: true}}
}

func (p *mysqlParser) signal() stmt {
	t := p.next()
	s := &raiseStmt{line: t.line, level: "EXCEPTION"}
	if t.up == "RESIGNAL" {
		if !p.isOp(";") && !p.isWord("END") {
			p.refuse(t.line, "RESIGNAL that changes the error")
		}
		s.reraise = true
		return s
	}
	if p.acceptWord("SQLSTATE") {
		p.acceptWord("VALUE")
		v := p.next()
		if v.kind != tString {
			p.fail("expected the SQLSTATE in quotes, found %s", v.describe())
		}
		s.sqlstate = strings.ToUpper(v.text)
	} else {
		s.name = strings.ToUpper(p.ident().name)
	}
	if p.acceptWord("SET") {
		for {
			item := p.next()
			p.expectOp("=")
			value := p.expr()
			switch item.up {
			case "MESSAGE_TEXT":
				s.message = value
			case "MYSQL_ERRNO":
				// The number is MySQL's own; the SQLSTATE carries over.
			default:
				p.refuse(item.line, "SIGNAL that sets %s", item.up)
			}
			if !p.acceptOp(",") {
				break
			}
		}
	}
	return s
}

// decls reads the DECLARE statements a block starts with.
func (p *mysqlParser) decls(b *block) {
	for p.isWord("DECLARE") {
		t := p.next()
		switch {
		case p.isWord("CONTINUE", "EXIT", "UNDO") && p.peekAt(1).is("HANDLER"):
			action := p.next().up
			p.pos++
			if action == "UNDO" {
				p.refuse(t.line, "an UNDO handler")
			}
			p.expectWord("FOR")
			h := &handler{action: action, line: t.line}
			for {
				h.conds = append(h.conds, p.condition())
				if !p.acceptOp(",") {
					break
				}
			}
			body := p.statement()
			if blk, ok := body.(*blockStmt); ok && len(blk.b.decls) == 0 && len(blk.b.handlers) == 0 && blk.b.label == "" {
				h.body = blk.b.body
			} else {
				h.body = []stmt{body}
			}
			b.handlers = append(b.handlers, h)
		case p.peekAt(1).is("CONDITION"):
			name := p.ident().name
			p.pos++
			p.expectWord("FOR")
			c := p.condition()
			b.decls = append(b.decls, &exceptionDecl{name: name, cond: &c, line: t.line})
		case p.peekAt(1).is("CURSOR"):
			name := p.ident().name
			p.pos++
			p.expectWord("FOR")
			b.decls = append(b.decls, &cursorDecl{name: name, query: p.query(), line: t.line})
		default:
			var names []string
			for {
				names = append(names, p.ident().name)
				if !p.acceptOp(",") {
					break
				}
			}
			typ := p.parseType()
			var def expr
			if p.acceptWord("DEFAULT") {
				def = p.expr()
			}
			for _, n := range names {
				b.decls = append(b.decls, &varDecl{name: n, typ: typ, def: def, line: t.line})
			}
		}
		p.expectOp(";")
	}
}

// condition reads what a handler is for.
func (p *mysqlParser) condition() condition {
	t := p.next()
	switch {
	case t.up == "SQLEXCEPTION":
		return condition{kind: "others"}
	case t.up == "SQLWARNING":
		return condition{kind: "warning"}
	case t.up == "NOT" && p.acceptWord("FOUND"):
		return condition{kind: "notfound"}
	case t.up == "SQLSTATE":
		p.acceptWord("VALUE")
		v := p.next()
		if v.kind != tString {
			p.fail("expected the SQLSTATE in quotes, found %s", v.describe())
		}
		return condition{kind: "sqlstate", value: strings.ToUpper(v.text)}
	case t.kind == tNumber:
		return condition{kind: "code", value: t.text}
	case t.kind == tWord || t.kind == tQuoted:
		return condition{kind: "name", value: strings.ToUpper(t.text)}
	}
	p.pos--
	p.fail("expected a condition, found %s", t.describe())
	return condition{}
}
