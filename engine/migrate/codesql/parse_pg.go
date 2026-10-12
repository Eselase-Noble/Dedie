package codesql

import (
	"fmt"
	"strings"
)

// pgParser reads a PostgreSQL function or procedure as
// pg_get_functiondef prints it, written in PL/pgSQL or in SQL. A trigger
// is given as its CREATE TRIGGER statement together with the function it
// executes, because in PostgreSQL the two are separate objects and
// neither says enough on its own.
type pgParser struct {
	*parser
	// lang is the language of the function being read.
	lang string
}

func parsePostgres(src string) (r *routine, err error) {
	toks, err := lex(Postgres, src)
	if err != nil {
		return nil, err
	}
	p := &pgParser{parser: &parser{d: Postgres, toks: toks}}
	defer catch(&err)
	var head *triggerHead
	for p.pos < len(p.toks) {
		if p.acceptOp(";") {
			continue
		}
		p.expectWord("CREATE")
		p.acceptWords("OR", "REPLACE")
		switch {
		case p.isWord("CONSTRAINT"):
			p.refuse(p.peek().line, "a constraint trigger")
		case p.acceptWord("TRIGGER"):
			if head != nil {
				p.fail("the text holds two triggers")
			}
			head = p.triggerHead()
		case p.isWord("FUNCTION", "PROCEDURE"):
			if r != nil {
				p.fail("the text holds two routines")
			}
			r = p.routine()
		default:
			p.fail("expected FUNCTION, PROCEDURE or TRIGGER, found %s", p.peek().describe())
		}
	}
	switch {
	case r == nil && head != nil:
		p.fail("the trigger comes without the function it executes; give both, separated by a semicolon")
	case r == nil:
		p.fail("expected FUNCTION, PROCEDURE or TRIGGER")
	case head != nil:
		if r.returns == nil || r.returns.name != "TRIGGER" {
			p.fail("the function given with the trigger does not return trigger")
		}
		r.kind, r.trigger, r.returns = "trigger", head, nil
		r.name = head.name
	case r.returns != nil && r.returns.name == "TRIGGER":
		p.refuse(1, "a trigger function on its own: it is converted together with each trigger that executes it")
	case r.returns != nil && r.returns.name == "EVENT_TRIGGER":
		p.refuse(1, "an event trigger")
	}
	return r, nil
}

func (p *pgParser) triggerHead() *triggerHead {
	h := &triggerHead{line: p.peek().line, newName: "NEW", oldName: "OLD"}
	h.name = p.qualified()
	t := p.next()
	switch t.up {
	case "BEFORE", "AFTER":
		h.timing = t.up
	case "INSTEAD":
		p.expectWord("OF")
		h.timing = "INSTEAD OF"
	default:
		p.pos--
		p.fail("expected BEFORE, AFTER or INSTEAD OF, found %s", t.describe())
	}
	for {
		ev := p.next()
		if !dmlEvents[ev.up] {
			p.refuse(ev.line, "a trigger on %s", ev.up)
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
	h.table = p.qualified()
	for {
		switch {
		case p.isWord("REFERENCING"):
			p.refuse(p.peek().line, "a trigger with transition tables (REFERENCING)")
		case p.isWord("FROM"), p.isWord("DEFERRABLE"), p.isWord("INITIALLY"), p.isWord("NOT") && p.peekAt(1).is("DEFERRABLE"):
			p.refuse(p.peek().line, "a constraint trigger")
		case p.acceptWord("FOR"):
			p.acceptWord("EACH")
			h.forEachRow = p.next().up == "ROW"
		case p.acceptWord("WHEN"):
			p.expectOp("(")
			h.when = p.expr()
			p.expectOp(")")
		case p.acceptWord("EXECUTE"):
			p.next()
			p.qualified()
			p.expectOp("(")
			if !p.isOp(")") {
				p.refuse(p.peek().line, "a trigger that passes arguments to its function")
			}
			p.expectOp(")")
			return h
		default:
			p.fail("unexpected %s in the trigger", p.peek().describe())
		}
	}
}

// pgTypeStarts are the first words of the built-in types, used to tell a
// parameter that has no name from one whose name is followed by a type.
var pgTypeStarts = words("INTEGER INT INT2 INT4 INT8 BIGINT SMALLINT NUMERIC DECIMAL REAL DOUBLE FLOAT FLOAT4 FLOAT8 " +
	"TEXT VARCHAR CHARACTER CHAR BPCHAR BOOLEAN BOOL DATE TIMESTAMP TIMESTAMPTZ TIME TIMETZ INTERVAL UUID JSON JSONB " +
	"BYTEA RECORD ANYELEMENT ANYARRAY REFCURSOR OID NAME XML MONEY INET CIDR BIT")

func (p *pgParser) routine() *routine {
	t := p.next()
	r := &routine{kind: strings.ToLower(t.up)}
	r.name = p.qualified()
	p.expectOp("(")
	for !p.isOp(")") {
		tk := p.peek()
		pm := &param{mode: "IN", line: tk.line}
		switch {
		case p.acceptWord("INOUT"):
			pm.mode = "INOUT"
		case p.acceptWord("OUT"):
			pm.mode = "OUT"
		case p.isWord("VARIADIC"):
			p.refuse(tk.line, "a VARIADIC parameter")
		case p.isWord("IN") && p.peekAt(1).kind == tWord && !p.peekAt(1).op(","):
			p.pos++
			if p.acceptWord("OUT") {
				pm.mode = "INOUT"
			}
		}
		// A name is there when the word is not the start of a type, or
		// when another word follows that can only be a type.
		named := p.peek().kind == tQuoted
		if p.peek().kind == tWord {
			next := p.peekAt(1)
			named = (next.kind == tWord || next.kind == tQuoted) && !next.is("DEFAULT") &&
				!(pgTypeStarts[p.peek().up] && (next.is("PRECISION") || next.is("VARYING") || next.is("WITH") || next.is("WITHOUT")))
		}
		if named {
			pm.name = p.ident().name
		} else {
			pm.name = fmt.Sprintf("p%d", len(r.params)+1)
		}
		pm.typ = p.parseType()
		if p.acceptWord("DEFAULT") || p.acceptOp("=") {
			pm.def = p.expr()
		}
		r.params = append(r.params, pm)
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(")")
	var body *token
	for body == nil {
		tk := p.peek()
		switch {
		case tk.kind == tEOF, tk.op(";"):
			p.fail("the routine has no body")
		case p.acceptWord("RETURNS"):
			r.returns = p.parseType()
			if r.returns.name == "VOID" {
				r.returns = nil
				r.kind = "procedure"
				r.voidFunction = true
			}
		case p.acceptWord("LANGUAGE"):
			p.lang = strings.ToLower(p.next().text)
		case p.acceptWord("IMMUTABLE"):
			r.deterministic = true
		case p.acceptWord("STABLE", "VOLATILE", "LEAKPROOF", "WINDOW"):
		case p.acceptWord("STRICT"), p.acceptWords("RETURNS", "NULL", "ON", "NULL", "INPUT"):
			r.strict = true
		case p.acceptWords("CALLED", "ON", "NULL", "INPUT"):
		case p.acceptWords("NOT", "LEAKPROOF"):
		case p.acceptWord("SECURITY"), p.acceptWord("PARALLEL"), p.acceptWord("COST"), p.acceptWord("ROWS"), p.acceptWord("SUPPORT"):
			p.pos++
		case p.acceptWord("SET"):
			// A setting for the duration of the call; search_path is
			// the usual one and means nothing to the target.
			for !p.isWord("AS", "LANGUAGE", "SET", "IMMUTABLE", "STABLE", "VOLATILE", "STRICT", "SECURITY", "PARALLEL", "COST", "ROWS", "RETURN", "BEGIN") && p.peek().kind != tEOF {
				p.pos++
			}
		case p.acceptWord("AS"):
			s := p.next()
			if s.kind != tString {
				p.fail("expected the body of the routine in quotes, found %s", s.describe())
			}
			if p.isOp(",") {
				p.refuse(s.line, "a routine written in C")
			}
			body = &s
		case tk.is("RETURN"), tk.is("BEGIN"):
			// A body in the SQL standard's form, not in quotes.
			r.sqlBody = true
			r.body = p.standardBody()
			return p.checked(r)
		default:
			p.fail("unexpected %s in the routine's header", tk.describe())
		}
	}
	// The options may also follow the body.
	for p.pos < len(p.toks) && !p.isOp(";") {
		if p.acceptWord("LANGUAGE") {
			p.lang = strings.ToLower(p.next().text)
			continue
		}
		p.pos++
	}
	toks, err := lex(Postgres, body.text)
	if err != nil {
		if le, ok := err.(*lexError); ok {
			le.line += body.line - 1
		}
		panic(&parseError{body.line, err.Error()})
	}
	for i := range toks {
		toks[i].line += body.line - 1
	}
	inner := &pgParser{parser: &parser{d: Postgres, toks: toks}, lang: p.lang}
	switch p.lang {
	case "plpgsql":
		r.body, r.conflict = inner.plpgsql()
	case "sql":
		r.sqlBody = true
		r.body = inner.sqlBody(false)
	default:
		p.refuse(t.line, "a routine written in %s", p.lang)
	}
	return p.checked(r)
}

// checked refuses what no later stage can make sense of.
func (p *pgParser) checked(r *routine) *routine {
	if r.returns != nil && (r.returns.setOf || r.returns.name == "RECORD") {
		p.refuse(r.returns.line, "a function that returns rows (%s)", map[bool]string{true: "SETOF or TABLE", false: "record"}[r.returns.setOf])
	}
	return r
}

// standardBody reads "RETURN expr" or "BEGIN ATOMIC ... END".
func (p *pgParser) standardBody() *block {
	t := p.next()
	if t.up == "RETURN" {
		return &block{line: t.line, body: []stmt{&returnStmt{value: p.expr(), line: t.line}}}
	}
	p.expectWord("ATOMIC")
	b := p.sqlBody(true)
	p.expectWord("END")
	return b
}

// sqlBody reads the statements of a function in the SQL language. The
// result of the last one is the function's result.
func (p *pgParser) sqlBody(atomic bool) *block {
	b := &block{line: p.peek().line}
	for p.pos < len(p.toks) && !(atomic && p.isWord("END")) {
		if p.acceptOp(";") {
			continue
		}
		t := p.peek()
		if atomic && p.acceptWord("RETURN") {
			b.body = append(b.body, &returnStmt{value: p.expr(), line: t.line})
			continue
		}
		if !p.isWord("SELECT", "WITH", "INSERT", "UPDATE", "DELETE") {
			p.refuse(t.line, "the statement %s in a function written in SQL", t.describe())
		}
		b.body = append(b.body, &sqlExec{sql: p.sqlStatementPG(), line: t.line})
	}
	// The result of the last statement, when it is a query, is what the
	// function returns: its first row.
	if n := len(b.body); n > 0 {
		if last, ok := b.body[n-1].(*sqlExec); ok && last.sql.verb == "SELECT" {
			q := last.sql
			var value expr = &subExpr{sub: q, line: last.line}
			switch {
			case len(q.columns) != 1:
				p.refuse(last.line, "a function in SQL whose last query gives several columns")
			case len(q.tables) == 0 && len(q.parts) == 2:
				// SELECT expression, and nothing else.
				value = q.columns[0].e
			case q.plain && !singleRow(q):
				q.parts = append(q.parts, sqlPart{limit: &limitPart{count: &litExpr{kind: "number", text: "1", line: last.line}}})
			}
			b.body[n-1] = &returnStmt{value: value, line: last.line}
		}
	}
	return b
}

func (p *pgParser) sqlStatementPG() *sqlStmt {
	if p.isWord("INSERT", "UPDATE", "DELETE") {
		s := p.sqlStatement()
		if p.isWord("RETURNING") {
			p.refuse(p.peek().line, "RETURNING")
		}
		return s
	}
	return p.query()
}

// plpgsql reads a PL/pgSQL body: the directives, then the outer block.
func (p *pgParser) plpgsql() (*block, string) {
	conflict := ""
	for p.acceptOp("#") {
		name := p.next()
		switch name.up {
		case "VARIABLE_CONFLICT":
			conflict = strings.ToLower(p.next().text)
		case "OPTION", "PRINT_STRICT_PARAMS":
			p.pos++
		default:
			p.fail("the directive #%s is not known", name.text)
		}
	}
	s := p.statement()
	b, ok := s.(*blockStmt)
	if !ok {
		p.fail("the body is not a block")
	}
	if p.pos < len(p.toks) {
		p.fail("unexpected %s after the end of the body", p.peek().describe())
	}
	return b.b, conflict
}

func (p *pgParser) decls() []decl {
	var out []decl
	for !p.isWord("BEGIN") {
		t := p.peek()
		if t.kind == tEOF {
			p.fail("expected BEGIN, found the end of the text")
		}
		if p.acceptWord("DECLARE") {
			continue
		}
		name := p.ident().name
		switch {
		case p.acceptWord("ALIAS"):
			p.refuse(t.line, "an alias for a parameter (%s ALIAS FOR)", name)
		case p.isWord("CURSOR"), (p.isWord("SCROLL") || p.isWord("NO")) && (p.peekAt(1).is("CURSOR") || p.peekAt(2).is("CURSOR")):
			if p.acceptWord("SCROLL") {
				p.refuse(t.line, "a scrollable cursor")
			}
			p.acceptWords("NO", "SCROLL")
			p.expectWord("CURSOR")
			c := &cursorDecl{name: name, line: t.line}
			if p.acceptOp("(") {
				for {
					pt := p.peek()
					pm := &param{name: p.ident().name, mode: "IN", line: pt.line}
					pm.typ = p.parseType()
					c.params = append(c.params, pm)
					if !p.acceptOp(",") {
						break
					}
				}
				p.expectOp(")")
			}
			if !p.acceptWord("FOR", "IS") {
				p.fail("expected FOR after CURSOR")
			}
			c.query = p.query()
			p.expectOp(";")
			out = append(out, c)
		default:
			v := &varDecl{name: name, line: t.line}
			v.constant = p.acceptWord("CONSTANT")
			v.typ = p.parseType()
			if p.acceptWords("NOT", "NULL") {
				v.notNull = true
			}
			if p.acceptOp(":=") || p.acceptOp("=") || p.acceptWord("DEFAULT") {
				v.def = p.expr()
			}
			p.expectOp(";")
			out = append(out, v)
		}
	}
	return out
}

func (p *pgParser) stmts() []stmt {
	var out []stmt
	for !p.isWord("END", "EXCEPTION", "ELSIF", "ELSEIF", "ELSE", "WHEN") {
		if p.peek().kind == tEOF {
			p.fail("expected END, found the end of the text")
		}
		out = append(out, p.statement())
	}
	return out
}

func (p *pgParser) statement() stmt {
	label := ""
	if p.acceptOp("<<") {
		label = p.ident().name
		p.expectOp(">>")
	}
	t := p.peek()
	assignment := p.peekAt(1).op(":=") || p.peekAt(1).op("=") || (p.peekAt(1).op(".") && (p.peekAt(3).op(":=") || p.peekAt(3).op("=")))
	if t.kind == tWord && !assignment {
		if s := p.keywordStatement(t, label); s != nil {
			return s
		}
	}
	if t.kind != tWord && t.kind != tQuoted {
		p.fail("expected a statement, found %s", t.describe())
	}
	// What is left is an assignment.
	target := &nameExpr{parts: p.qualified(), line: t.line}
	if p.isOp("[") {
		p.refuse(t.line, "an assignment to an array element")
	}
	if !p.acceptOp(":=") && !p.acceptOp("=") {
		p.fail("expected a statement, found %s", t.describe())
	}
	s := &assignStmt{target: target, value: p.expr(), line: t.line}
	p.expectOp(";")
	return s
}

func (p *pgParser) keywordStatement(t token, label string) stmt {
	endLabel := func() {
		if !p.isOp(";") {
			p.ident()
		}
		p.expectOp(";")
	}
	switch t.up {
	case "DECLARE", "BEGIN":
		b := &block{label: label, line: t.line}
		if p.isWord("DECLARE") {
			b.decls = p.decls()
		}
		p.expectWord("BEGIN")
		b.body = p.stmts()
		if p.acceptWord("EXCEPTION") {
			for p.isWord("WHEN") {
				h := &handler{line: p.next().line, action: "EXIT"}
				for {
					c := p.next()
					switch {
					case c.up == "OTHERS":
						h.conds = append(h.conds, condition{kind: "others"})
					case c.up == "SQLSTATE":
						v := p.next()
						if v.kind != tString {
							p.fail("expected the SQLSTATE in quotes")
						}
						h.conds = append(h.conds, condition{kind: "sqlstate", value: strings.ToUpper(v.text)})
					case c.kind == tWord:
						h.conds = append(h.conds, condition{kind: "name", value: c.up})
					default:
						p.pos--
						p.fail("expected the name of an error, found %s", c.describe())
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
		if p.pos < len(p.toks) {
			endLabel()
		}
		return &blockStmt{b}
	case "IF":
		p.pos++
		s := &ifStmt{line: t.line}
		for {
			br := ifBranch{cond: p.expr()}
			p.expectWord("THEN")
			br.body = p.stmts()
			s.branches = append(s.branches, br)
			if !p.acceptWord("ELSIF", "ELSEIF") {
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
			var br caseBranch
			for {
				br.values = append(br.values, p.expr())
				if !p.acceptOp(",") {
					break
				}
			}
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
		p.expectOp(";")
		return s
	case "LOOP", "WHILE", "FOR":
		return p.loop(label)
	case "FOREACH":
		p.refuse(t.line, "FOREACH over an array")
	case "EXIT", "CONTINUE":
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
	case "RETURN":
		p.pos++
		if p.isWord("NEXT", "QUERY") {
			p.refuse(t.line, "RETURN %s: the function returns rows", p.peek().up)
		}
		s := &returnStmt{line: t.line}
		if !p.isOp(";") {
			s.value = p.expr()
		}
		p.expectOp(";")
		return s
	case "RAISE":
		return p.raise()
	case "NULL":
		p.pos++
		p.expectOp(";")
		return &nullStmt{line: t.line}
	case "PERFORM":
		p.pos++
		s := &performStmt{value: p.expr(), line: t.line}
		if !p.isOp(";") {
			p.refuse(t.line, "PERFORM of a query")
		}
		p.pos++
		return s
	case "EXECUTE":
		p.refuse(t.line, "dynamic SQL (EXECUTE): the statement is built while the routine runs and cannot be translated ahead of time")
	case "GET":
		p.pos++
		if p.isWord("STACKED") {
			p.refuse(t.line, "GET STACKED DIAGNOSTICS")
		}
		p.acceptWord("CURRENT")
		p.expectWord("DIAGNOSTICS")
		target := &nameExpr{parts: p.qualified(), line: t.line}
		if !p.acceptOp("=") && !p.acceptOp(":=") {
			p.fail("expected = in GET DIAGNOSTICS")
		}
		if item := p.next(); item.up != "ROW_COUNT" {
			p.refuse(t.line, "GET DIAGNOSTICS of %s", item.up)
		}
		if p.isOp(",") {
			p.refuse(t.line, "GET DIAGNOSTICS of more than the row count")
		}
		p.expectOp(";")
		return &diagStmt{target: target, line: t.line}
	case "OPEN":
		p.pos++
		s := &openStmt{cursor: p.ident().name, line: t.line}
		if p.isWord("FOR", "SCROLL", "NO") {
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
		if p.acceptWord("NEXT") {
			p.acceptWord("FROM", "IN")
		} else if p.isWord("PRIOR", "FIRST", "LAST", "ABSOLUTE", "RELATIVE", "FORWARD", "BACKWARD") {
			p.refuse(t.line, "FETCH %s", p.peek().up)
		} else if p.isWord("FROM") && p.peekAt(1).kind == tWord && !p.peekAt(1).is("INTO") {
			p.pos++
		}
		s := &fetchStmt{cursor: p.ident().name, line: t.line}
		p.expectWord("INTO")
		for {
			s.into = append(s.into, p.postfix())
			if !p.acceptOp(",") {
				break
			}
		}
		p.expectOp(";")
		return s
	case "MOVE":
		p.refuse(t.line, "MOVE")
	case "CLOSE":
		p.pos++
		s := &closeStmt{cursor: p.ident().name, line: t.line}
		p.expectOp(";")
		return s
	case "CALL":
		p.pos++
		s := &callStmt{name: p.qualified(), line: t.line}
		p.expectOp("(")
		for !p.isOp(")") {
			s.args = append(s.args, p.expr())
			s.argNames = append(s.argNames, "")
			if !p.acceptOp(",") {
				break
			}
		}
		p.expectOp(")")
		p.expectOp(";")
		return s
	case "COMMIT", "ROLLBACK":
		p.pos++
		if p.isWord("AND") {
			p.refuse(t.line, "%s AND CHAIN", t.up)
		}
		p.expectOp(";")
		return &txStmt{verb: t.up, line: t.line}
	case "ASSERT":
		p.refuse(t.line, "ASSERT")
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE":
		s := &sqlExec{sql: p.sqlStatement(), line: t.line}
		p.expectOp(";")
		return s
	case "MERGE", "TRUNCATE", "CREATE", "DROP", "ALTER", "LOCK", "SET", "RESET", "COPY", "NOTIFY", "LISTEN", "ANALYZE", "VACUUM", "REFRESH", "GRANT", "REVOKE", "SAVEPOINT", "RELEASE", "DO", "VALUES":
		p.refuse(t.line, "the statement %s", t.up)
	}
	return nil
}

func (p *pgParser) loop(label string) stmt {
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
		if p.isOp(",") {
			p.refuse(t.line, "a FOR loop over a query with several loop variables")
		}
		p.expectWord("IN")
		switch {
		case p.isWord("EXECUTE"):
			p.refuse(t.line, "a FOR loop over dynamic SQL (EXECUTE)")
		case p.isWord("SELECT", "WITH"):
			s.kind = "query"
			s.query = p.query()
		case p.isOp("(") && (p.peekAt(1).is("SELECT") || p.peekAt(1).is("WITH")):
			p.pos++
			s.kind = "query"
			s.query = p.query()
			p.expectOp(")")
		default:
			s.reverse = p.acceptWord("REVERSE")
			first := p.concat()
			if p.acceptOp("..") {
				s.kind = "range"
				s.lo, s.hi = first, p.concat()
				if s.reverse {
					// PL/pgSQL writes a descending range high to low;
					// the tree always has the low bound first.
					s.lo, s.hi = s.hi, s.lo
				}
				if p.acceptWord("BY") {
					s.step = p.concat()
				}
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

// raise reads RAISE in its several forms.
func (p *pgParser) raise() stmt {
	t := p.next()
	s := &raiseStmt{line: t.line, level: "EXCEPTION"}
	if p.acceptOp(";") {
		s.reraise = true
		return s
	}
	if p.isWord("DEBUG", "LOG", "INFO", "NOTICE", "WARNING", "EXCEPTION") {
		s.level = p.next().up
	}
	switch {
	case p.peek().kind == tString:
		s.format, s.hasFmt = p.next().text, true
		for p.acceptOp(",") {
			s.args = append(s.args, p.expr())
		}
	case p.acceptWord("SQLSTATE"):
		v := p.next()
		if v.kind != tString {
			p.fail("expected the SQLSTATE in quotes")
		}
		s.sqlstate = strings.ToUpper(v.text)
	case p.isWord("USING"), p.isOp(";"):
	default:
		s.name = strings.ToUpper(p.ident().name)
	}
	if p.acceptWord("USING") {
		for {
			opt := p.next()
			if !p.acceptOp("=") && !p.acceptOp(":=") {
				p.fail("expected = after %s", opt.describe())
			}
			value := p.expr()
			switch opt.up {
			case "MESSAGE":
				s.message = value
			case "ERRCODE":
				code, ok := isLit(value, "string")
				if !ok {
					p.refuse(t.line, "RAISE with an error code that is not a literal")
				}
				if len(code) == 5 && strings.ToUpper(code) == code {
					s.sqlstate = code
				} else {
					s.name = strings.ToUpper(code)
				}
			case "DETAIL", "HINT":
				s.dropped = append(s.dropped, opt.up)
			default:
				p.refuse(t.line, "RAISE ... USING %s", opt.up)
			}
			if !p.acceptOp(",") {
				break
			}
		}
	}
	p.expectOp(";")
	return s
}
