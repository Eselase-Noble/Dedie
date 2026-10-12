package codesql

import (
	"fmt"
	"strconv"
	"strings"
)

// parser holds the tokens of one routine and the place reading has got
// to. The three language parsers share it, together with the parsers for
// expressions, SQL statements and types, which are the same language in
// all three apart from details the dialect field decides.
type parser struct {
	d    Dialect
	toks []token
	pos  int
}

// parseError says the text is not what the parser expected. It carries
// the line so the note about it can name the place.
type parseError struct {
	line int
	msg  string
}

func (e *parseError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// unsupportedError says the text was understood and has no translation.
type unsupportedError struct {
	line int
	what string
}

func (e *unsupportedError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.what) }

// fail stops parsing: the routine is not in a form this parser reads.
// The panic is caught where parsing starts.
func (p *parser) fail(format string, args ...any) {
	panic(&parseError{p.peek().line, fmt.Sprintf(format, args...)})
}

// refuse stops parsing at a construct that was recognised and cannot be
// carried over.
func (p *parser) refuse(line int, format string, args ...any) {
	panic(&unsupportedError{line, fmt.Sprintf(format, args...)})
}

var eof = token{kind: tEOF}

func (p *parser) peek() token { return p.peekAt(0) }

func (p *parser) peekAt(n int) token {
	if p.pos < 0 {
		// A parser that stepped back from the end of an empty text.
		p.pos = 0
	}
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	e := eof
	if len(p.toks) > 0 {
		e.line = p.toks[len(p.toks)-1].line
	}
	return e
}

func (p *parser) next() token {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *parser) isWord(words ...string) bool {
	t := p.peek()
	if t.kind != tWord {
		return false
	}
	for _, w := range words {
		if t.up == w {
			return true
		}
	}
	return false
}

func (p *parser) isOp(op string) bool { return p.peek().op(op) }

// acceptWord consumes the next token if it is one of the words.
func (p *parser) acceptWord(words ...string) bool {
	if p.isWord(words...) {
		p.pos++
		return true
	}
	return false
}

// acceptWords consumes a run of words if all of them follow.
func (p *parser) acceptWords(words ...string) bool {
	for i, w := range words {
		if !p.peekAt(i).is(w) {
			return false
		}
	}
	p.pos += len(words)
	return true
}

func (p *parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectWord(word string) token {
	if !p.isWord(word) {
		p.fail("expected %s, found %s", word, p.peek().describe())
	}
	return p.next()
}

func (p *parser) expectOp(op string) token {
	if !p.isOp(op) {
		p.fail("expected %q, found %s", op, p.peek().describe())
	}
	return p.next()
}

// isName reports whether the next token can be an identifier.
func (p *parser) isName() bool {
	k := p.peek().kind
	return k == tWord || k == tQuoted
}

// ident reads one identifier.
func (p *parser) ident() ident {
	t := p.peek()
	if t.kind != tWord && t.kind != tQuoted {
		p.fail("expected a name, found %s", t.describe())
	}
	p.pos++
	return ident{name: t.text, quoted: t.kind == tQuoted}
}

// qualified reads a name with its qualifiers: a, a.b, a.b.c.
func (p *parser) qualified() []ident {
	parts := []ident{p.ident()}
	for p.isOp(".") && (p.peekAt(1).kind == tWord || p.peekAt(1).kind == tQuoted) {
		p.pos++
		parts = append(parts, p.ident())
	}
	return parts
}

// skipTo moves past tokens until one of the words is next, at the
// nesting depth reading started at. It reports whether it found one.
func (p *parser) skipTo(words ...string) bool {
	depth := 0
	for ; p.pos < len(p.toks); p.pos++ {
		t := p.toks[p.pos]
		switch {
		case t.op("("):
			depth++
		case t.op(")"):
			depth--
		case depth == 0 && t.kind == tWord:
			for _, w := range words {
				if t.up == w {
					return true
				}
			}
		}
	}
	return false
}

// Types.

// typeWords are the type names of more than one word, by their first.
var typeWords = map[string][][]string{
	"DOUBLE":    {{"PRECISION"}},
	"CHARACTER": {{"VARYING"}},
	"CHAR":      {{"VARYING"}},
	"LONG":      {{"RAW"}},
	"NATIONAL":  {{"CHARACTER", "VARYING"}, {"CHAR", "VARYING"}, {"CHARACTER"}, {"CHAR"}, {"VARCHAR"}},
}

// parseType reads a data type. Anything it does not understand as a
// type is still read as far as it goes; whether the type can be
// translated is decided later, where the message can say what it was for.
func (p *parser) parseType() *dataType {
	d := &dataType{line: p.peek().line}
	if p.d == Postgres && p.acceptWord("SETOF") {
		d.setOf = true
	}
	first := p.peek()
	if first.kind != tWord && first.kind != tQuoted {
		p.fail("expected a type, found %s", first.describe())
	}
	// name%TYPE, table.column%TYPE and table%ROWTYPE.
	if q := p.lookQualified(); p.peekAt(q).op("%") && (p.peekAt(q+1).is("TYPE") || p.peekAt(q+1).is("ROWTYPE")) {
		d.ref = p.qualified()
		p.pos++
		d.rowtype = p.next().up == "ROWTYPE"
		return d
	}
	p.pos++
	d.name = strings.ToUpper(first.text)
	if p.d == Postgres && d.name == "TABLE" && p.isOp("(") {
		d.setOf = true
		p.skipParens()
		return d
	}
	for _, more := range typeWords[d.name] {
		if p.acceptWords(more...) {
			d.name += " " + strings.Join(more, " ")
			break
		}
	}
	if p.isOp(".") {
		// A type of a schema or a package: nothing built in.
		for p.acceptOp(".") {
			d.name += "." + strings.ToUpper(p.ident().name)
		}
	}
	if d.name == "INTERVAL" {
		// INTERVAL YEAR TO MONTH, INTERVAL DAY(2) TO SECOND(6).
		for p.isWord("YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "SECOND", "TO") {
			d.name += " " + p.next().up
			if p.isOp("(") {
				p.skipParens()
			}
		}
		return d
	}
	if p.isOp("(") {
		p.pos++
		for !p.isOp(")") {
			t := p.next()
			switch {
			case t.kind == tNumber:
				n, err := strconv.Atoi(t.text)
				if err != nil {
					p.fail("the size of a type is not a whole number")
				}
				d.args = append(d.args, n)
			case t.op("-") && p.peek().kind == tNumber:
				n, _ := strconv.Atoi(p.next().text)
				d.args = append(d.args, -n)
			case t.op("*"):
				d.star = true
			case t.op(","), t.is("CHAR"), t.is("BYTE"):
			case t.kind == tString && (d.name == "ENUM" || d.name == "SET"):
			default:
				p.pos--
				p.fail("unexpected %s in the size of a type", t.describe())
			}
		}
		p.pos++
	}
	if d.name == "TIMESTAMP" || d.name == "TIME" {
		switch {
		case p.acceptWords("WITH", "LOCAL", "TIME", "ZONE"):
			d.name += " WITH LOCAL TIME ZONE"
		case p.acceptWords("WITH", "TIME", "ZONE"):
			d.name += " WITH TIME ZONE"
		case p.acceptWords("WITHOUT", "TIME", "ZONE"):
		}
	}
	for {
		switch {
		case p.d == MySQL && p.acceptWord("UNSIGNED"):
			d.unsigned = true
		case p.d == MySQL && p.acceptWord("SIGNED", "ZEROFILL"):
		case p.d == MySQL && p.acceptWords("CHARACTER", "SET"), p.d == MySQL && p.acceptWord("CHARSET"), p.d != Oracle && p.acceptWord("COLLATE"):
			p.ident()
		case p.d == Postgres && p.isOp("[") && p.peekAt(1).op("]"):
			p.pos += 2
			d.array = true
		default:
			return d
		}
	}
}

// lookQualified returns how many tokens a qualified name starting at
// the current position takes, without consuming it.
func (p *parser) lookQualified() int {
	n := 1
	for p.peekAt(n).op(".") && (p.peekAt(n+1).kind == tWord || p.peekAt(n+1).kind == tQuoted) {
		n += 2
	}
	return n
}

// skipParens moves past a parenthesised group that starts here.
func (p *parser) skipParens() {
	depth := 0
	for p.pos < len(p.toks) {
		t := p.next()
		if t.op("(") {
			depth++
		} else if t.op(")") {
			if depth--; depth == 0 {
				return
			}
		}
	}
	p.fail("a parenthesis is never closed")
}

// parseTypeText reads a type from text that is nothing but the type, as
// a catalog gives it.
func parseTypeText(d Dialect, text string) (dt *dataType, ok bool) {
	toks, err := lex(d, text)
	if err != nil || len(toks) == 0 {
		return nil, false
	}
	defer func() {
		if recover() != nil {
			dt, ok = nil, false
		}
	}()
	p := &parser{d: d, toks: toks}
	dt = p.parseType()
	return dt, p.pos == len(toks)
}

// Expressions.

// valueStops are the words that can never begin a value. Meeting one
// where a value should be means the text is not what the parser thinks
// it is, which is better reported than read as a name.
var valueStops = map[string]bool{
	"FROM": true, "WHERE": true, "THEN": true, "ELSE": true, "END": true, "LOOP": true, "INTO": true,
	"AND": true, "OR": true, "WHEN": true, "AS": true, "IS": true, "IN": true, "BEGIN": true, "ELSIF": true,
	"ELSEIF": true, "GROUP": true, "ORDER": true, "HAVING": true, "UNION": true, "SET": true, "VALUES": true,
	"DO": true, "UNTIL": true, "ON": true, "JOIN": true, "USING": true, "RETURNING": true, "LIKE": true,
	"BETWEEN": true, "DECLARE": true, "EXCEPTION": true,
}

// expr reads a whole expression.
func (p *parser) expr() expr { return p.orExpr() }

func (p *parser) orExpr() expr {
	l := p.andExpr()
	for {
		t := p.peek()
		switch {
		case t.is("OR"), p.d == MySQL && t.op("||"):
			p.pos++
			l = &binExpr{op: "OR", l: l, r: p.andExpr(), line: t.line}
		case p.d == MySQL && t.is("XOR"):
			p.pos++
			p.andExpr()
			l = &rawExpr{what: "the XOR operator", line: t.line}
		default:
			return l
		}
	}
}

func (p *parser) andExpr() expr {
	l := p.notExpr()
	for {
		t := p.peek()
		if t.is("AND") || (p.d == MySQL && t.op("&&")) {
			p.pos++
			l = &binExpr{op: "AND", l: l, r: p.notExpr(), line: t.line}
			continue
		}
		return l
	}
}

func (p *parser) notExpr() expr {
	t := p.peek()
	if t.is("NOT") {
		p.pos++
		return &unExpr{op: "NOT", x: p.notExpr(), line: t.line}
	}
	return p.comparison()
}

var comparisonOps = map[string]string{"=": "=", "<>": "<>", "!=": "<>", "^=": "<>", "~=": "<>", "<": "<", "<=": "<=", ">": ">", ">=": ">="}

func (p *parser) comparison() expr {
	l := p.concat()
	for {
		t := p.peek()
		if t.kind == tOp {
			if op, ok := comparisonOps[t.text]; ok {
				p.pos++
				if p.isWord("ANY", "ALL", "SOME") {
					p.refuse(t.line, "a comparison with ANY or ALL")
				}
				l = &binExpr{op: op, l: l, r: p.concat(), line: t.line}
				continue
			}
			if p.d == MySQL && t.text == "<=>" {
				p.pos++
				l = &binExpr{op: "IS NOT DISTINCT FROM", l: l, r: p.concat(), line: t.line}
				continue
			}
			if p.d == Postgres && (t.text == "~" || t.text == "!") && l != nil {
				p.refuse(t.line, "a regular expression operator")
			}
			return l
		}
		if t.kind != tWord {
			return l
		}
		not := false
		save := p.pos
		if t.up == "NOT" && (p.peekAt(1).is("IN") || p.peekAt(1).is("LIKE") || p.peekAt(1).is("BETWEEN") || p.peekAt(1).is("ILIKE")) {
			not = true
			p.pos++
			t = p.peek()
		}
		switch t.up {
		case "IS":
			p.pos++
			n := p.acceptWord("NOT")
			switch {
			case p.acceptWord("NULL"):
				l = &isExpr{x: l, not: n, what: "NULL", line: t.line}
			case p.d != Oracle && p.acceptWord("TRUE"):
				l = &isExpr{x: l, not: n, what: "TRUE", line: t.line}
			case p.d != Oracle && p.acceptWord("FALSE"):
				l = &isExpr{x: l, not: n, what: "FALSE", line: t.line}
			case p.d == Postgres && p.acceptWords("DISTINCT", "FROM"):
				op := "IS DISTINCT FROM"
				if n {
					op = "IS NOT DISTINCT FROM"
				}
				l = &binExpr{op: op, l: l, r: p.concat(), line: t.line}
			default:
				p.refuse(t.line, "the test IS %s", p.peek().describe())
			}
		case "BETWEEN":
			p.pos++
			lo := p.concat()
			p.expectWord("AND")
			l = &betweenExpr{x: l, lo: lo, hi: p.concat(), not: not, line: t.line}
		case "IN":
			p.pos++
			in := &inExpr{x: l, not: not, line: t.line}
			p.expectOp("(")
			if p.isWord("SELECT", "WITH") {
				in.sub = p.query()
			} else {
				for {
					in.list = append(in.list, p.expr())
					if !p.acceptOp(",") {
						break
					}
				}
			}
			p.expectOp(")")
			l = in
		case "LIKE", "ILIKE":
			if t.up == "ILIKE" && p.d != Postgres {
				return l
			}
			p.pos++
			lk := &likeExpr{x: l, not: not, insensitive: t.up == "ILIKE", line: t.line}
			lk.pattern = p.concat()
			if p.acceptWord("ESCAPE") {
				lk.escape = p.concat()
			}
			l = lk
		case "REGEXP", "RLIKE", "SIMILAR", "SOUNDS", "MEMBER", "SUBMULTISET":
			p.refuse(t.line, "the %s operator", t.up)
		default:
			p.pos = save
			return l
		}
	}
}

// concat reads the operators between comparisons and multiplication.
// Oracle gives "||" the precedence of "+" and "-"; PostgreSQL ranks it
// below them. MySQL has no such operator: there "||" means OR.
func (p *parser) concat() expr {
	if p.d != Postgres {
		return p.additive()
	}
	l := p.additive()
	for p.isOp("||") {
		t := p.next()
		l = &binExpr{op: "||", l: l, r: p.additive(), line: t.line}
	}
	return l
}

func (p *parser) additive() expr {
	l := p.term()
	for {
		t := p.peek()
		switch {
		case t.op("+"), t.op("-"):
			p.pos++
			l = &binExpr{op: t.text, l: l, r: p.term(), line: t.line}
		case p.d == Oracle && t.op("||"):
			p.pos++
			l = &binExpr{op: "||", l: l, r: p.term(), line: t.line}
		case t.op("->"), t.op("->>"), t.op("&"), t.op("|"), t.op("<<") && p.d == MySQL, t.op(">>") && p.d == MySQL:
			p.refuse(t.line, "the %s operator", t.text)
		default:
			return l
		}
	}
}

func (p *parser) term() expr {
	l := p.unary()
	for {
		t := p.peek()
		switch {
		case t.op("*"), t.op("/"):
			p.pos++
			l = &binExpr{op: t.text, l: l, r: p.unary(), line: t.line}
		case t.op("%") && p.d != Oracle:
			p.pos++
			l = &binExpr{op: "%", l: l, r: p.unary(), line: t.line}
		case p.d == MySQL && t.is("DIV"):
			p.pos++
			l = &binExpr{op: "DIV", l: l, r: p.unary(), line: t.line}
		case p.d == MySQL && t.is("MOD"):
			p.pos++
			l = &binExpr{op: "%", l: l, r: p.unary(), line: t.line}
		default:
			return l
		}
	}
}

func (p *parser) unary() expr {
	t := p.peek()
	if t.op("-") || t.op("+") {
		p.pos++
		return &unExpr{op: t.text, x: p.unary(), line: t.line}
	}
	if p.d == MySQL && t.op("!") {
		// MySQL's "!" binds like a sign, tighter than NOT.
		p.pos++
		return &unExpr{op: "NOT", x: p.unary(), line: t.line}
	}
	if p.d == MySQL && t.is("BINARY") && p.peekAt(1).kind != tOp {
		p.refuse(t.line, "the BINARY operator")
	}
	if p.d == Oracle && t.is("PRIOR") {
		p.refuse(t.line, "a hierarchical query (CONNECT BY)")
	}
	return p.power()
}

func (p *parser) power() expr {
	l := p.postfix()
	t := p.peek()
	if (p.d == Postgres && t.op("^")) || (p.d == Oracle && t.op("**")) {
		p.pos++
		return &binExpr{op: "^", l: l, r: p.unary(), line: t.line}
	}
	if p.d == MySQL && t.op("^") {
		p.refuse(t.line, "the bitwise ^ operator")
	}
	return l
}

func (p *parser) postfix() expr {
	e := p.primary()
	for {
		t := p.peek()
		switch {
		case p.d == Postgres && t.op("::"):
			p.pos++
			e = &castExpr{x: e, typ: p.parseType(), line: t.line}
		case p.d == Postgres && t.op("["):
			p.refuse(t.line, "an array element")
		case p.d != Oracle && t.is("COLLATE"):
			p.refuse(t.line, "a COLLATE clause")
		case p.d == Oracle && t.op("(") && p.peekAt(1).op("+") && p.peekAt(2).op(")"):
			p.refuse(t.line, "an outer join written with (+)")
		default:
			return e
		}
	}
}

func (p *parser) primary() expr {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.pos++
		return &litExpr{kind: "number", text: t.text, line: t.line}
	case tString:
		p.pos++
		return &litExpr{kind: "string", text: t.text, line: t.line}
	case tBind:
		p.pos++
		n := &nameExpr{parts: []ident{{name: t.text}}, bind: true, line: t.line}
		for p.isOp(".") && (p.peekAt(1).kind == tWord || p.peekAt(1).kind == tQuoted) {
			p.pos++
			n.parts = append(n.parts, p.ident())
		}
		return n
	case tParam:
		p.pos++
		return &nameExpr{parts: []ident{{name: "$" + t.text}}, line: t.line}
	case tUserVar:
		p.pos++
		return &rawExpr{what: "the session variable @" + t.text, line: t.line}
	case tOp:
		switch t.text {
		case "(":
			p.pos++
			if p.isWord("SELECT", "WITH") {
				s := p.query()
				p.expectOp(")")
				return &subExpr{sub: s, line: t.line}
			}
			pe := &parenExpr{line: t.line}
			for {
				pe.items = append(pe.items, p.expr())
				if !p.acceptOp(",") {
					break
				}
			}
			p.expectOp(")")
			return pe
		case "*":
			p.pos++
			return &starExpr{line: t.line}
		}
		p.fail("expected a value, found %q", t.text)
	case tEOF:
		p.fail("expected a value, found the end of the text")
	}
	if t.kind == tWord {
		if e := p.keywordValue(t); e != nil {
			return e
		}
		if valueStops[t.up] {
			p.fail("expected a value, found %s", t.text)
		}
	}
	return p.nameOrCall()
}

// keywordValue reads the values that begin with a keyword: literals with
// a type in front, CASE, CAST and the functions with a syntax of their
// own. It returns nil when the word is an ordinary name.
func (p *parser) keywordValue(t token) expr {
	next := p.peekAt(1)
	switch t.up {
	case "NULL":
		p.pos++
		return &litExpr{kind: "null", line: t.line}
	case "TRUE", "FALSE":
		p.pos++
		return &litExpr{kind: "bool", text: t.up, line: t.line}
	case "DATE", "TIMESTAMP":
		if next.kind == tString {
			p.pos += 2
			return &litExpr{kind: strings.ToLower(t.up), text: next.text, line: t.line}
		}
	case "INTERVAL":
		if next.kind == tString && p.d != MySQL {
			p.pos += 2
			lit := &litExpr{kind: "interval", text: next.text, line: t.line}
			for p.isWord("YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "SECOND", "TO") {
				lit.unit = strings.TrimSpace(lit.unit + " " + p.next().up)
				if p.isOp("(") {
					p.skipParens()
				}
			}
			return lit
		}
		if p.d == MySQL {
			p.pos++
			n := p.expr()
			unit := p.next()
			if unit.kind != tWord {
				p.fail("expected the unit of an interval, found %s", unit.describe())
			}
			return &intervalExpr{n: n, unit: unit.up, line: t.line}
		}
	case "CASE":
		p.pos++
		c := &caseExpr{line: t.line}
		if !p.isWord("WHEN") {
			c.operand = p.expr()
		}
		for p.acceptWord("WHEN") {
			w := caseWhen{cond: p.expr()}
			p.expectWord("THEN")
			w.result = p.expr()
			c.whens = append(c.whens, w)
		}
		if p.acceptWord("ELSE") {
			c.els = p.expr()
		}
		p.expectWord("END")
		return c
	case "CAST":
		if next.op("(") {
			p.pos += 2
			x := p.expr()
			p.expectWord("AS")
			c := &castExpr{x: x, line: t.line}
			if p.d == MySQL {
				c.typ = p.mysqlCastType()
			} else {
				c.typ = p.parseType()
			}
			p.expectOp(")")
			return c
		}
	case "EXISTS":
		if next.op("(") {
			p.pos += 2
			s := p.query()
			p.expectOp(")")
			return &existsExpr{sub: s, line: t.line}
		}
	case "EXTRACT":
		if next.op("(") {
			p.pos += 2
			f := p.next()
			if f.kind != tWord && f.kind != tString {
				p.fail("expected the field to extract, found %s", f.describe())
			}
			p.expectWord("FROM")
			e := &extractExpr{field: strings.ToUpper(f.text), x: p.expr(), line: t.line}
			p.expectOp(")")
			return e
		}
	case "TRIM":
		if next.op("(") {
			return p.trimCall(t)
		}
	case "SUBSTRING", "SUBSTR":
		if next.op("(") && p.d != Oracle {
			// SUBSTRING(s FROM start FOR length) beside the usual form.
			save := p.pos
			p.pos += 2
			s := p.expr()
			if p.acceptWord("FROM") {
				c := &callExpr{name: []ident{{name: "SUBSTRING"}}, args: []expr{s, p.expr()}, line: t.line}
				if p.acceptWord("FOR") {
					c.args = append(c.args, p.expr())
				}
				p.expectOp(")")
				c.argNames = make([]string, len(c.args))
				return c
			}
			p.pos = save
		}
	case "POSITION":
		if next.op("(") && p.d != Oracle {
			p.pos += 2
			needle := p.concat()
			p.expectWord("IN")
			c := &callExpr{name: []ident{{name: "POSITION"}}, args: []expr{needle, p.expr()}, argNames: make([]string, 2), line: t.line}
			p.expectOp(")")
			return c
		}
	case "SELECT", "WITH":
		p.fail("a query stands where a value should be")
	}
	return nil
}

// trimCall reads TRIM in its SQL form and writes it as a call with the
// string first and what to remove second: TRIM, LTRIM or RTRIM.
func (p *parser) trimCall(t token) expr {
	p.pos += 2
	name := "TRIM"
	switch {
	case p.acceptWord("LEADING"):
		name = "LTRIM"
	case p.acceptWord("TRAILING"):
		name = "RTRIM"
	case p.acceptWord("BOTH"):
	}
	c := &callExpr{name: []ident{{name: name}}, line: t.line}
	if p.acceptWord("FROM") {
		c.args = []expr{p.expr()}
	} else {
		first := p.expr()
		if p.acceptWord("FROM") {
			c.args = []expr{p.expr(), first}
		} else {
			c.args = []expr{first}
			for p.acceptOp(",") {
				c.args = append(c.args, p.expr())
			}
		}
	}
	p.expectOp(")")
	c.argNames = make([]string, len(c.args))
	return c
}

// mysqlCastType reads the target of a MySQL CAST, whose type names are a
// small list of their own, and names it as the declared type that means
// the same.
func (p *parser) mysqlCastType() *dataType {
	t := p.peek()
	switch {
	case p.acceptWord("SIGNED"):
		p.acceptWord("INTEGER", "INT")
		return &dataType{name: "BIGINT", line: t.line}
	case p.acceptWord("UNSIGNED"):
		p.acceptWord("INTEGER", "INT")
		return &dataType{name: "BIGINT", unsigned: true, line: t.line}
	}
	d := p.parseType()
	if d.name == "CHAR" && len(d.args) == 0 {
		// CAST(x AS CHAR) gives a string of any length.
		d.name = "TEXT"
	}
	return d
}

// nameOrCall reads a reference or a function call.
func (p *parser) nameOrCall() expr {
	t := p.peek()
	n := &nameExpr{line: t.line}
	n.parts = append(n.parts, p.ident())
	for p.isOp(".") {
		if p.peekAt(1).op("*") {
			p.pos += 2
			return &starExpr{qualifier: n.parts, line: t.line}
		}
		if k := p.peekAt(1).kind; k != tWord && k != tQuoted {
			break
		}
		p.pos++
		n.parts = append(n.parts, p.ident())
	}
	if p.d == Oracle && p.isOp("%") && p.peekAt(1).kind == tWord {
		p.pos++
		attr := p.next()
		if len(n.parts) != 1 {
			p.refuse(t.line, "the attribute %%%s of a qualified name", attr.up)
		}
		return &attrExpr{cursor: strings.ToUpper(n.parts[0].name), attr: attr.up, line: t.line}
	}
	if p.d == Oracle && p.isOp("@") {
		p.refuse(t.line, "a database link")
	}
	if !p.isOp("(") {
		return n
	}
	if p.d == Oracle && p.peekAt(1).op("+") && p.peekAt(2).op(")") {
		p.refuse(t.line, "an outer join written with (+)")
	}
	p.pos++
	c := &callExpr{name: n.parts, line: t.line}
	if p.acceptWord("DISTINCT", "UNIQUE") {
		c.distinct = true
	} else {
		p.acceptWord("ALL")
	}
	for !p.isOp(")") {
		name := ""
		if p.peek().kind == tWord && p.peekAt(1).op("=>") {
			name = p.next().text
			p.pos++
		} else if p.d == Postgres && p.peek().kind == tWord && p.peekAt(1).op(":=") {
			name = p.next().text
			p.pos++
		}
		if p.isOp("*") && (p.peekAt(1).op(")")) {
			p.pos++
			c.star = true
			break
		}
		c.args = append(c.args, p.expr())
		c.argNames = append(c.argNames, name)
		if p.isWord("ORDER") && p.peekAt(1).is("BY") {
			p.pos += 2
			c.withinOrder = p.orderKeys()
		}
		if p.isWord("SEPARATOR") {
			p.refuse(p.peek().line, "GROUP_CONCAT with a SEPARATOR")
		}
		if !p.acceptOp(",") {
			break
		}
	}
	p.expectOp(")")
	if p.acceptWords("WITHIN", "GROUP") {
		p.expectOp("(")
		p.expectWord("ORDER")
		p.expectWord("BY")
		c.withinOrder = p.orderKeys()
		p.expectOp(")")
	}
	if p.isWord("FILTER") && p.peekAt(1).op("(") {
		p.refuse(p.peek().line, "an aggregate with a FILTER clause")
	}
	if p.isWord("OVER") && p.peekAt(1).op("(") {
		p.pos += 2
		c.over = []sqlPart{{kw: "("}}
		if p.acceptWords("PARTITION", "BY") {
			c.over = append(c.over, sqlPart{kw: "PARTITION BY"})
			for {
				c.over = append(c.over, sqlPart{e: p.expr()})
				if !p.acceptOp(",") {
					break
				}
				c.over = append(c.over, sqlPart{kw: ","})
			}
		}
		if p.acceptWords("ORDER", "BY") {
			c.over = append(c.over, sqlPart{kw: "ORDER BY"})
			c.over = append(c.over, p.orderKeys()...)
		}
		if !p.isOp(")") {
			p.refuse(p.peek().line, "a window frame (%s ...)", p.peek().describe())
		}
		p.pos++
		c.over = append(c.over, sqlPart{kw: ")"})
	}
	return c
}

// SQL statements.

// clauseWords are the words that end a table name or a select item, so
// that what follows them is not taken for an alias.
var clauseWords = map[string]bool{
	"FROM": true, "WHERE": true, "SET": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true, "FULL": true,
	"CROSS": true, "NATURAL": true, "ON": true, "USING": true, "GROUP": true, "ORDER": true, "HAVING": true,
	"UNION": true, "INTERSECT": true, "MINUS": true, "EXCEPT": true, "LIMIT": true, "OFFSET": true, "FETCH": true,
	"FOR": true, "CONNECT": true, "START": true, "VALUES": true, "SELECT": true, "RETURNING": true, "LOOP": true,
	"INTO": true, "WINDOW": true, "LOCK": true, "PARTITION": true, "WHEN": true, "THEN": true, "BULK": true,
	"AS": true, "IS": true, "DO": true, "OUTER": true, "STRAIGHT_JOIN": true, "MODEL": true, "PIVOT": true,
	"UNPIVOT": true, "WITH": true, "LATERAL": true, "END": true, "DEFAULT": true, "ELSE": true,
}

// sqlStatement reads the SQL statement that starts here, up to but not
// including the semicolon that ends it.
func (p *parser) sqlStatement() *sqlStmt {
	t := p.peek()
	switch t.up {
	case "SELECT", "WITH":
		return p.query()
	case "INSERT":
		return p.insert()
	case "UPDATE":
		return p.update()
	case "DELETE":
		return p.delete()
	}
	p.fail("expected an SQL statement, found %s", t.describe())
	return nil
}

func kw(s string) sqlPart { return sqlPart{kw: s} }

// query reads a SELECT with everything that may follow it.
func (p *parser) query() *sqlStmt {
	s := &sqlStmt{verb: "SELECT", line: p.peek().line, simple: true, plain: true}
	if p.acceptWord("WITH") {
		if p.isWord("RECURSIVE") {
			p.refuse(s.line, "a recursive query")
		}
		s.parts = append(s.parts, kw("WITH"))
		for {
			name := p.ident()
			p.expectWord("AS")
			p.expectOp("(")
			s.parts = append(s.parts, sqlPart{alias: &name}, kw("AS ("), sqlPart{sub: p.query()}, kw(")"))
			p.expectOp(")")
			if !p.acceptOp(",") {
				break
			}
			s.parts = append(s.parts, kw(","))
		}
		s.simple = false
	}
	p.expectWord("SELECT")
	s.parts = append(s.parts, kw("SELECT"))
	if p.acceptWord("DISTINCT", "UNIQUE") {
		if p.isWord("ON") {
			p.refuse(s.line, "SELECT DISTINCT ON")
		}
		s.parts = append(s.parts, kw("DISTINCT"))
		s.simple = false
	} else {
		p.acceptWord("ALL")
	}
	if p.d == MySQL && p.isWord("SQL_CALC_FOUND_ROWS", "SQL_NO_CACHE", "STRAIGHT_JOIN", "HIGH_PRIORITY") {
		p.refuse(s.line, "the %s modifier", p.peek().up)
	}
	for {
		e := p.expr()
		item := selectItem{e: e}
		s.parts = append(s.parts, sqlPart{e: e})
		switch {
		case p.acceptWord("AS"):
			a := p.ident()
			s.parts = append(s.parts, sqlPart{alias: &a})
			item.name = a.name
		case p.isName() && !(p.peek().kind == tWord && clauseWords[p.peek().up]):
			a := p.ident()
			s.parts = append(s.parts, sqlPart{alias: &a})
			item.name = a.name
		default:
			if n, ok := e.(*nameExpr); ok {
				item.name = n.parts[len(n.parts)-1].name
			}
		}
		s.columns = append(s.columns, item)
		if !p.acceptOp(",") {
			break
		}
		s.parts = append(s.parts, kw(","))
	}
	p.intoClause(s)
	if p.acceptWord("FROM") {
		s.parts = append(s.parts, kw("FROM"))
		p.fromList(s)
	}
	if p.acceptWord("WHERE") {
		if p.isWord("CURRENT") && p.peekAt(1).is("OF") {
			p.refuse(p.peek().line, "WHERE CURRENT OF in a query")
		}
		s.parts = append(s.parts, kw("WHERE"), sqlPart{e: p.expr(), cond: true})
	}
	if p.d == Oracle && (p.isWord("CONNECT") || (p.isWord("START") && p.peekAt(1).is("WITH"))) {
		p.refuse(p.peek().line, "a hierarchical query (CONNECT BY)")
	}
	if p.acceptWords("GROUP", "BY") {
		s.simple = false
		s.parts = append(s.parts, kw("GROUP BY"))
		if p.isWord("ROLLUP", "CUBE", "GROUPING") {
			p.refuse(p.peek().line, "GROUP BY %s", p.peek().up)
		}
		p.exprList(s)
		if p.isWord("WITH") && p.peekAt(1).is("ROLLUP") {
			p.refuse(p.peek().line, "GROUP BY WITH ROLLUP")
		}
	}
	if p.acceptWord("HAVING") {
		s.simple = false
		s.parts = append(s.parts, kw("HAVING"), sqlPart{e: p.expr(), cond: true})
	}
	if p.isWord("WINDOW") {
		p.refuse(p.peek().line, "a WINDOW clause")
	}
	if p.isWord("UNION", "INTERSECT", "MINUS", "EXCEPT") {
		op := p.next().up
		if op == "MINUS" {
			op = "EXCEPT"
		}
		if p.acceptWord("ALL") {
			op += " ALL"
		} else {
			p.acceptWord("DISTINCT")
		}
		s.simple, s.plain = false, false
		s.parts = append(s.parts, kw(op), sqlPart{sub: p.query()})
		// Whatever follows a set operation was read as part of its
		// last arm, which is also where it is written again.
		p.intoClause(s)
		return s
	}
	if p.acceptWords("ORDER", "BY") {
		s.simple = false
		if p.isWord("SIBLINGS") {
			p.refuse(p.peek().line, "a hierarchical query (ORDER SIBLINGS BY)")
		}
		s.parts = append(s.parts, kw("ORDER BY"))
		s.parts = append(s.parts, p.orderKeys()...)
	}
	p.limitClause(s)
	if p.isWord("FOR") && (p.peekAt(1).is("UPDATE") || p.peekAt(1).is("SHARE")) {
		p.pos++
		lock := "FOR " + p.next().up
		if p.isWord("OF") {
			p.refuse(p.peek().line, "FOR UPDATE OF")
		}
		switch {
		case p.acceptWord("NOWAIT"):
			lock += " NOWAIT"
		case p.acceptWords("SKIP", "LOCKED"):
			lock += " SKIP LOCKED"
		case p.isWord("WAIT"):
			p.refuse(p.peek().line, "FOR UPDATE WAIT")
		}
		s.parts = append(s.parts, kw(lock))
	}
	if p.d == MySQL && p.isWord("LOCK") {
		p.refuse(p.peek().line, "LOCK IN SHARE MODE")
	}
	// PL/pgSQL and MySQL also take INTO at the very end.
	p.intoClause(s)
	return s
}

// intoClause reads INTO and its variables, if they are written here.
func (p *parser) intoClause(s *sqlStmt) {
	if p.isWord("BULK") && p.peekAt(1).is("COLLECT") {
		p.refuse(p.peek().line, "BULK COLLECT")
	}
	if !p.isWord("INTO") || len(s.into) > 0 {
		return
	}
	p.pos++
	if p.d == Postgres && p.acceptWord("STRICT") {
		s.strict = true
	}
	if p.d == MySQL && p.isWord("OUTFILE", "DUMPFILE") {
		p.refuse(p.peek().line, "SELECT ... INTO OUTFILE")
	}
	for {
		s.into = append(s.into, p.postfix())
		if !p.acceptOp(",") {
			break
		}
	}
	s.parts = append(s.parts, sqlPart{into: true})
}

func (p *parser) exprList(s *sqlStmt) {
	for {
		s.parts = append(s.parts, sqlPart{e: p.expr()})
		if !p.acceptOp(",") {
			return
		}
		s.parts = append(s.parts, kw(","))
	}
}

func (p *parser) orderKeys() []sqlPart {
	var out []sqlPart
	for {
		k := &orderKey{e: p.expr()}
		if p.acceptWord("DESC") {
			k.desc = true
		} else {
			p.acceptWord("ASC")
		}
		if p.acceptWord("NULLS") {
			k.nulls = p.next().up
		}
		out = append(out, sqlPart{order: k})
		if !p.acceptOp(",") {
			return out
		}
		out = append(out, kw(","))
	}
}

// limitClause reads a row limit in any of the spellings.
func (p *parser) limitClause(s *sqlStmt) {
	var l *limitPart
	for {
		switch {
		case p.d != Oracle && p.acceptWord("LIMIT"):
			l = orLimit(l)
			if p.acceptWord("ALL") {
				continue
			}
			first := p.expr()
			if p.d == MySQL && p.acceptOp(",") {
				l.offset, l.count = first, p.expr()
			} else {
				l.count = first
			}
		case p.d != Oracle && p.acceptWord("OFFSET"):
			l = orLimit(l)
			l.offset = p.expr()
			p.acceptWord("ROW", "ROWS")
		case p.isWord("OFFSET") && p.d == Oracle:
			p.pos++
			l = orLimit(l)
			l.offset = p.expr()
			p.acceptWord("ROW", "ROWS")
		case p.isWord("FETCH") && (p.peekAt(1).is("FIRST") || p.peekAt(1).is("NEXT")):
			p.pos += 2
			l = orLimit(l)
			if p.isWord("ROW", "ROWS") {
				l.count = &litExpr{kind: "number", text: "1", line: p.peek().line}
			} else {
				l.count = p.postfix()
			}
			if p.isWord("PERCENT") {
				p.refuse(p.peek().line, "FETCH FIRST n PERCENT")
			}
			p.acceptWord("ROW", "ROWS")
			if !p.acceptWord("ONLY") {
				p.refuse(p.peek().line, "FETCH FIRST ... WITH TIES")
			}
		default:
			if l != nil {
				s.simple, s.plain = false, false
				s.parts = append(s.parts, sqlPart{limit: l})
			}
			return
		}
	}
}

func orLimit(l *limitPart) *limitPart {
	if l == nil {
		return &limitPart{}
	}
	return l
}

// fromList reads the tables of a FROM clause with their joins.
func (p *parser) fromList(s *sqlStmt) {
	for {
		p.tableRef(s)
		for {
			t := p.peek()
			if !p.isWord("JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "NATURAL", "STRAIGHT_JOIN") {
				break
			}
			var words []string
			for p.isWord("INNER", "LEFT", "RIGHT", "FULL", "CROSS", "NATURAL", "OUTER") {
				words = append(words, p.next().up)
			}
			if !p.acceptWord("JOIN") {
				p.refuse(t.line, "the join %s", strings.Join(append(words, p.peek().describe()), " "))
			}
			if p.isWord("LATERAL") {
				p.refuse(t.line, "a LATERAL join")
			}
			s.simple = false
			s.parts = append(s.parts, kw(strings.Join(append(words, "JOIN"), " ")))
			p.tableRef(s)
			switch {
			case p.acceptWord("ON"):
				s.parts = append(s.parts, kw("ON"), sqlPart{e: p.expr(), cond: true})
			case p.acceptWord("USING"):
				p.expectOp("(")
				s.parts = append(s.parts, kw("USING ("))
				for {
					c := p.ident()
					s.parts = append(s.parts, sqlPart{col: &c})
					if !p.acceptOp(",") {
						break
					}
					s.parts = append(s.parts, kw(","))
				}
				p.expectOp(")")
				s.parts = append(s.parts, kw(")"))
			}
		}
		if !p.acceptOp(",") {
			return
		}
		s.simple = false
		s.parts = append(s.parts, kw(","))
	}
}

// tableRef reads one table of a FROM clause, or the table a statement
// changes, with its alias.
func (p *parser) tableRef(s *sqlStmt) *tableRef {
	t := p.peek()
	ref := &tableRef{line: t.line}
	switch {
	case t.op("("):
		p.pos++
		if !p.isWord("SELECT", "WITH") {
			p.refuse(t.line, "a parenthesised join")
		}
		ref.sub = p.query()
		p.expectOp(")")
	case t.is("TABLE") && p.peekAt(1).op("("), t.is("LATERAL"), t.is("ONLY"), t.is("XMLTABLE"), t.is("JSON_TABLE"), t.is("UNNEST"):
		p.refuse(t.line, "%s in a FROM clause", t.up)
	default:
		ref.name = p.qualified()
		if p.isOp("(") && s.verb == "SELECT" {
			p.refuse(t.line, "the function %s used as a table", ref.name[len(ref.name)-1].name)
		}
		if p.isOp("@") {
			p.refuse(t.line, "a database link")
		}
		if p.isWord("PARTITION", "SAMPLE", "TABLESAMPLE") && (p.peekAt(1).op("(")) {
			p.refuse(t.line, "a %s clause", p.peek().up)
		}
	}
	if p.acceptWord("AS") {
		ref.alias = p.ident().name
	} else if p.isName() && !(p.peek().kind == tWord && clauseWords[p.peek().up]) {
		ref.alias = p.ident().name
	}
	s.tables = append(s.tables, ref)
	s.parts = append(s.parts, sqlPart{table: ref})
	return ref
}

// returningClause reads RETURNING ... INTO at the end of a change.
func (p *parser) returningClause(s *sqlStmt) {
	if !p.acceptWord("RETURNING") {
		if p.isWord("RETURN") && p.d == Oracle {
			p.refuse(p.peek().line, "RETURN ... INTO")
		}
		return
	}
	s.returning = true
	s.parts = append(s.parts, kw("RETURNING"))
	p.exprList(s)
	p.intoClause(s)
	if len(s.into) == 0 {
		p.refuse(s.line, "RETURNING without INTO")
	}
}

func (p *parser) whereClause(s *sqlStmt) {
	if !p.acceptWord("WHERE") {
		return
	}
	if p.acceptWords("CURRENT", "OF") {
		c := p.ident()
		s.parts = append(s.parts, kw("WHERE CURRENT OF"), sqlPart{col: &c})
		return
	}
	s.parts = append(s.parts, kw("WHERE"), sqlPart{e: p.expr(), cond: true})
}

func (p *parser) insert() *sqlStmt {
	s := &sqlStmt{verb: "INSERT", line: p.peek().line}
	p.expectWord("INSERT")
	if p.d == MySQL && p.isWord("IGNORE", "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY") {
		p.refuse(s.line, "INSERT %s", p.peek().up)
	}
	if p.d == Oracle && p.isWord("ALL", "FIRST") {
		p.refuse(s.line, "a multi-table INSERT")
	}
	if !p.acceptWord("INTO") && p.d != MySQL {
		p.fail("expected INTO, found %s", p.peek().describe())
	}
	s.parts = append(s.parts, kw("INSERT INTO"))
	if p.isOp("(") {
		p.refuse(s.line, "an INSERT into a subquery")
	}
	ref := p.tableRef(s)
	if ref.alias != "" && p.d != Oracle && p.d != Postgres {
		p.fail("unexpected %s after the table of an INSERT", ref.alias)
	}
	if p.isOp("(") && !p.peekAt(1).is("SELECT") && !p.peekAt(1).is("WITH") {
		p.pos++
		s.parts = append(s.parts, kw("("))
		for {
			c := p.qualified()
			s.parts = append(s.parts, sqlPart{col: &c[len(c)-1]})
			if !p.acceptOp(",") {
				break
			}
			s.parts = append(s.parts, kw(","))
		}
		p.expectOp(")")
		s.parts = append(s.parts, kw(")"))
	}
	switch {
	case p.acceptWord("VALUES"):
		s.parts = append(s.parts, kw("VALUES"))
		if !p.isOp("(") {
			p.refuse(s.line, "an INSERT of a whole record")
		}
		for {
			p.expectOp("(")
			s.parts = append(s.parts, kw("("))
			for {
				if p.acceptWord("DEFAULT") {
					s.parts = append(s.parts, kw("DEFAULT"))
				} else {
					s.parts = append(s.parts, sqlPart{e: p.expr()})
				}
				if !p.acceptOp(",") {
					break
				}
				s.parts = append(s.parts, kw(","))
			}
			p.expectOp(")")
			s.parts = append(s.parts, kw(")"))
			if !p.acceptOp(",") {
				break
			}
			s.parts = append(s.parts, kw(","))
		}
	case p.isWord("SELECT", "WITH"):
		s.parts = append(s.parts, sqlPart{sub: p.query()})
	case p.isOp("(") && (p.peekAt(1).is("SELECT") || p.peekAt(1).is("WITH")):
		p.pos++
		s.parts = append(s.parts, sqlPart{sub: p.query()})
		p.expectOp(")")
	default:
		p.refuse(s.line, "this form of INSERT (%s ...)", p.peek().describe())
	}
	if p.isWord("ON") {
		p.refuse(p.peek().line, "INSERT ... ON %s", p.peekAt(1).describe())
	}
	p.returningClause(s)
	return s
}

func (p *parser) update() *sqlStmt {
	s := &sqlStmt{verb: "UPDATE", line: p.peek().line}
	p.expectWord("UPDATE")
	if p.d == MySQL && p.isWord("IGNORE", "LOW_PRIORITY") {
		p.refuse(s.line, "UPDATE %s", p.peek().up)
	}
	s.parts = append(s.parts, kw("UPDATE"))
	if p.isOp("(") {
		p.refuse(s.line, "an UPDATE of a subquery")
	}
	p.tableRef(s)
	if !p.isWord("SET") {
		p.refuse(s.line, "an UPDATE of more than one table")
	}
	p.pos++
	s.parts = append(s.parts, kw("SET"))
	for {
		if p.isOp("(") {
			p.refuse(p.peek().line, "an UPDATE that sets several columns from one subquery")
		}
		c := p.qualified()
		if p.isWord("ROW") && len(c) == 1 && strings.EqualFold(c[0].name, "ROW") {
			p.refuse(p.peek().line, "an UPDATE of a whole record")
		}
		// A qualifier here can only be the table itself, and
		// PostgreSQL does not accept one.
		s.parts = append(s.parts, sqlPart{col: &c[len(c)-1]})
		p.expectOp("=")
		s.parts = append(s.parts, kw("="))
		if p.acceptWord("DEFAULT") {
			s.parts = append(s.parts, kw("DEFAULT"))
		} else {
			s.parts = append(s.parts, sqlPart{e: p.expr()})
		}
		if !p.acceptOp(",") {
			break
		}
		s.parts = append(s.parts, kw(","))
	}
	if p.isWord("FROM") {
		p.refuse(p.peek().line, "UPDATE ... FROM")
	}
	p.whereClause(s)
	if p.isWord("ORDER", "LIMIT") {
		p.refuse(p.peek().line, "an UPDATE with %s", p.peek().up)
	}
	p.returningClause(s)
	return s
}

func (p *parser) delete() *sqlStmt {
	s := &sqlStmt{verb: "DELETE", line: p.peek().line}
	p.expectWord("DELETE")
	if !p.acceptWord("FROM") && p.d != Oracle {
		p.refuse(s.line, "a DELETE from more than one table")
	}
	s.parts = append(s.parts, kw("DELETE FROM"))
	if p.isOp("(") {
		p.refuse(s.line, "a DELETE from a subquery")
	}
	p.tableRef(s)
	if p.isWord("USING") || p.isOp(",") {
		p.refuse(p.peek().line, "a DELETE that joins another table")
	}
	p.whereClause(s)
	if p.isWord("ORDER", "LIMIT") {
		p.refuse(p.peek().line, "a DELETE with %s", p.peek().up)
	}
	p.returningClause(s)
	return s
}
