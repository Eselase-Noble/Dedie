// Package viewsql carries view definitions from one SQL dialect to
// another, and checks afterwards that the result behaves like the
// original.
//
// The translation is deliberately narrow. A definition is split into
// tokens, so that string literals, quoted names and comments are never
// mistaken for SQL, and then rewritten construct by construct: names are
// requoted and mapped to what the migration calls them on the target, and
// a short list of functions and keywords is replaced by the target's
// spelling of the same thing. Every rewrite on that list means the same
// on both sides, including what it does with NULL. Anything that is not
// on the list stops the translation with the reason, and the view is left
// for the administrator: a view that is missing is noticed at once, one
// that quietly returns something else may not be noticed for months.
//
// Because no list of rewrites can know every difference between two
// databases (how they divide integers, for one), a translated view is
// also verified once source and target hold the same rows; see Finish.
package viewsql

import (
	"regexp"
	"strings"
)

// Ref names a table or view on the source.
type Ref struct{ Schema, Name string }

func (r Ref) String() string { return r.Schema + "." + r.Name }

// Relation is where a table or view of the source is found on the target.
type Relation struct {
	// Quoted is its name there in SQL, qualified where the target has
	// schemas.
	Quoted string
	// Bare is the name alone, unquoted. When it is not what the source
	// name maps to by itself, the translation gives the table that name
	// as an alias, so that columns qualified with it still resolve.
	Bare string
}

// Options say how one definition is to be translated.
type Options struct {
	From, To Dialect
	// Schema is the schema (in MySQL, the database) the view belongs to,
	// which is where a name without a schema is looked for.
	Schema string
	// Relation finds a table or view the definition reads. It reports
	// false for one the migration does not create on the target.
	Relation func(schema, name string) (Relation, bool)
	// Ident maps the name of a column or an alias to the name it has on
	// the target.
	Ident func(name string) string
	// Temporal reports whether some table or view the definition could
	// read has a date or time column of this name. It may be nil. MySQL
	// takes a date for a number in arithmetic, so that due + 30 is
	// accepted there and means nothing; with this the translator can tell
	// and leave such a view out.
	Temporal func(column string) bool
}

type pieceKind int

const (
	pKeyword pieceKind = iota
	pOp
	pComma
	// pPrimary is a complete operand: a name, a literal, a function
	// call, a bracketed expression or a whole CASE.
	pPrimary
)

type literalKind int

const (
	notLiteral literalKind = iota
	stringLiteral
	numberLiteral
	nullLiteral
)

// piece is a part of the output. Keeping the output in pieces, rather
// than as text, is what lets a later token reach back for its operand:
// the x of x::text, the operands of a || b.
type piece struct {
	kind pieceKind
	text string
	lit  literalKind
	// temporal says the operand is, or may be, a date or time.
	temporal bool
}

type translator struct {
	o     Options
	reads []Ref
	seen  map[Ref]bool
	// dates counts the dates and times written so far. An expression
	// during which it went up has one somewhere inside.
	dates int
}

// date writes an operand that is a date or time.
func (t *translator) date(text string) piece {
	t.dates++
	return piece{kind: pPrimary, text: text, temporal: true}
}

// column writes a reference to a column, which is a date if a column of
// that name is known to be one.
func (t *translator) column(text, name string) piece {
	if t.o.Temporal != nil && t.o.Temporal(name) {
		return t.date(text)
	}
	return piece{kind: pPrimary, text: text}
}

// Translate rewrites the query of a view for the target dialect. It
// returns the tables and views the query reads, by their source names. The
// error, when there is one, is an *Unsupported with the reason.
func Translate(definition string, o Options) (string, []Ref, error) {
	switch {
	case o.From == MySQL && o.To == Postgres, o.From == Postgres && o.To == MySQL,
		o.From == Oracle && o.To == Postgres, o.From == Oracle && o.To == MySQL:
	default:
		return "", nil, unsupported("views are not translated from %s to %s", o.From, o.To)
	}
	toks, err := lex(o.From, definition)
	if err != nil {
		return "", nil, err
	}
	nodes, err := nest(toks)
	if err != nil {
		return "", nil, err
	}
	if len(nodes) == 0 {
		return "", nil, unsupported("the source did not show its definition")
	}
	t := &translator{o: o, seen: map[Ref]bool{}}
	out, err := t.sequence(nodes, false, true)
	if err != nil {
		return "", nil, err
	}
	return out, t.reads, nil
}

// quote writes a name as a quoted identifier of the target.
func (t *translator) quote(name string) string {
	if t.o.To == MySQL {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ident is a column name or alias as it is written on the target.
func (t *translator) ident(name string) string { return t.quote(t.o.Ident(name)) }

// canon is the name a token stands for. An unquoted name is folded the
// way its dialect folds it; MySQL keeps the case as written.
func (t *translator) canon(tok token) string {
	if tok.kind == tQuoted {
		return tok.text
	}
	switch t.o.From {
	case Postgres:
		return strings.ToLower(tok.text)
	case Oracle:
		return strings.ToUpper(tok.text)
	}
	return tok.text
}

// literal writes a string value as a literal of the target.
func (t *translator) literal(value string) (piece, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return piece{}, unsupported("it has a string literal with a NUL character")
	}
	// Oracle has no empty string: '' is NULL there.
	if value == "" && t.o.From == Oracle {
		return piece{kind: pPrimary, text: "NULL", lit: nullLiteral}, nil
	}
	quoted := strings.ReplaceAll(value, "'", "''")
	if strings.Contains(value, `\`) {
		quoted = strings.ReplaceAll(quoted, `\`, `\\`)
		// MySQL reads a backslash as an escape in every literal;
		// PostgreSQL only in the E'...' form, whatever its settings.
		if t.o.To == Postgres {
			return piece{kind: pPrimary, text: "E'" + quoted + "'", lit: stringLiteral}, nil
		}
	}
	return piece{kind: pPrimary, text: "'" + quoted + "'", lit: stringLiteral}, nil
}

func render(pieces []piece) string {
	var b strings.Builder
	for i, p := range pieces {
		if i > 0 && p.kind != pComma {
			b.WriteByte(' ')
		}
		b.WriteString(p.text)
	}
	return b.String()
}

// sequence translates the tokens of one bracket level. from says the
// level is a bracketed part of a FROM clause, where a table is expected
// first; top says it is the statement itself.
func (t *translator) sequence(nodes []node, from, top bool) (string, error) {
	s := &seq{t: t, nodes: nodes, inFrom: from, expectRel: from, top: top}
	if err := s.run(); err != nil {
		return "", err
	}
	out, err := t.finish(s.out)
	if err != nil {
		return "", err
	}
	return render(out), nil
}

// expr translates an expression that is not a query of its own, such as
// one argument of a function.
func (t *translator) expr(nodes []node) (string, error) { return t.sequence(nodes, false, false) }

// seq is the state of translating one bracket level.
type seq struct {
	t     *translator
	nodes []node
	i     int
	out   []piece
	top   bool
	// sawSelect says a query has begun at this level, which is what makes
	// a FROM the start of its table list. inFrom says the tokens are
	// inside that list, and expectRel that the next name is a table.
	sawSelect, inFrom, expectRel bool
}

func (s *seq) peek(ahead int) *node {
	if s.i+ahead < len(s.nodes) {
		return &s.nodes[s.i+ahead]
	}
	return nil
}

func (s *seq) push(kind pieceKind, text string) { s.out = append(s.out, piece{kind: kind, text: text}) }

func (s *seq) last() *piece {
	if len(s.out) == 0 {
		return nil
	}
	return &s.out[len(s.out)-1]
}

func (s *seq) run() error {
	for s.i < len(s.nodes) {
		n := &s.nodes[s.i]
		var err error
		switch {
		case n.isGroup('('):
			err = s.group(n)
		case n.isGroup('['):
			err = unsupported("it uses an array")
		case n.isString():
			var p piece
			if p, err = s.t.literal(n.tok.text); err == nil {
				s.out = append(s.out, p)
				s.i++
			}
		case n.isNumber():
			s.out = append(s.out, piece{kind: pPrimary, text: n.tok.text, lit: numberLiteral})
			s.i++
		case n.tok.kind == tQuoted:
			err = s.chain()
		case n.tok.kind == tWord:
			err = s.word(n)
		default:
			err = s.operator(n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// group translates a bracketed expression, list, subquery or part of a
// FROM clause.
func (s *seq) group(n *node) error {
	if len(n.items) == 1 && n.items[0].isOp("+") {
		return unsupported("it uses Oracle's (+) outer join")
	}
	subquery := len(n.items) > 0 && (n.items[0].isWord("SELECT") || n.items[0].isWord("WITH"))
	inner := &seq{t: s.t, nodes: n.items}
	dates := s.t.dates
	if s.expectRel && !subquery {
		// A bracketed join, which PostgreSQL writes around every join.
		inner.inFrom, inner.expectRel = true, true
	}
	if err := inner.run(); err != nil {
		return err
	}
	out, err := s.t.finish(inner.out)
	if err != nil {
		return err
	}
	p := piece{kind: pPrimary, text: "(" + render(out) + ")", temporal: s.t.dates > dates}
	// (0) is still the literal 0 to whatever is applied to it.
	if len(out) == 1 {
		p.lit = out[0].lit
	}
	s.out = append(s.out, p)
	s.expectRel = false
	s.i++
	return nil
}

var (
	dateRe      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	timestampRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?$`)
)

// plainKeywords are written to the target as they are: they mean the
// same in all three dialects.
var plainKeywords = map[string]bool{
	"ALL": true, "AS": true, "AND": true, "OR": true, "NOT": true, "IN": true, "IS": true, "BETWEEN": true, "EXISTS": true,
	"CASE": true, "WHEN": true, "THEN": true, "ELSE": true, "BY": true, "ASC": true, "DESC": true, "ESCAPE": true,
	"INNER": true, "LEFT": true, "RIGHT": true, "OUTER": true, "CROSS": true,
}

// callable are the keywords that are also the names of functions; with
// an argument list behind them they are a call.
var callable = map[string]bool{"LEFT": true, "RIGHT": true}

// reserved are the words each source dialect keeps for itself and this
// translator has no rule for, with what to call the construct in the
// reason. A word that is not a keyword, not reserved and not a function is
// taken for a name, so these lists hold what could otherwise slip through
// as one. MySQL needs none: it writes every name of a stored definition in
// backticks, so any bare word left over is refused.
var reserved = map[Dialect]map[string]string{
	Postgres: {
		"WITH": "a WITH clause", "WINDOW": "a window function", "NATURAL": "a NATURAL join", "LATERAL": "a LATERAL join",
		"FETCH": "FETCH FIRST", "FOR": "FOR", "ONLY": "ONLY", "INTO": "INTO", "COLLATE": "COLLATE", "ANY": "ANY", "SOME": "SOME",
		"ARRAY": "an array", "ILIKE": "ILIKE", "SIMILAR": "SIMILAR TO", "VARIADIC": "VARIADIC", "ISNULL": "ISNULL", "NOTNULL": "NOTNULL",
		"SYMMETRIC": "BETWEEN SYMMETRIC", "OVERLAPS": "OVERLAPS", "TABLESAMPLE": "TABLESAMPLE", "UNIQUE": "UNIQUE", "BINARY": "BINARY",
		"USER": "the current user", "CURRENT_USER": "the current user", "SESSION_USER": "the current user",
		"CURRENT_TIME": "CURRENT_TIME", "LOCALTIME": "LOCALTIME",
	},
	Oracle: {
		"WITH": "a WITH clause", "CONNECT": "a hierarchical query (CONNECT BY)", "PRIOR": "a hierarchical query (CONNECT BY)",
		"START": "a hierarchical query (START WITH)", "LEVEL": "LEVEL", "ROWNUM": "ROWNUM", "ROWID": "ROWID", "UNIQUE": "UNIQUE",
		"ANY": "ANY", "SOME": "SOME", "FOR": "FOR", "INTO": "INTO", "UID": "the current user", "USER": "the current user",
		"NATURAL": "a NATURAL join", "LATERAL": "a LATERAL join", "SIBLINGS": "a hierarchical query (ORDER SIBLINGS BY)",
	},
}

// constructs are words that begin something with no rewrite when a
// bracket follows them, and are ordinary names otherwise.
var constructs = map[string]string{
	"OVER": "a window function (OVER)", "FILTER": "an aggregate FILTER", "PARTITION": "PARTITION", "KEEP": "KEEP (DENSE_RANK ...)",
	"PIVOT": "PIVOT", "UNPIVOT": "UNPIVOT", "SAMPLE": "SAMPLE", "ROW": "a row value", "VALUES": "VALUES", "ROLLUP": "ROLLUP",
	"CUBE": "CUBE", "GROUPING": "GROUPING", "CURSOR": "a cursor expression", "APPLY": "APPLY", "TABLE": "a table function",
	"UNNEST": "an array", "MATCH": "a full-text search", "INTERVAL": "an interval",
}

// refusal says whether a word stands for something this translator does
// not carry over, and what to call it. called says a bracket follows.
func (t *translator) refusal(up string, called bool) (string, bool) {
	if what, ok := reserved[t.o.From][up]; ok {
		return what, true
	}
	if what, ok := constructs[up]; ok && called {
		return what, true
	}
	return "", false
}

// now is the current moment, spelled for each target. The key is the
// word in the source, which takes no argument list there.
var now = map[Dialect]map[string]map[Dialect]string{
	Oracle: {
		// SYSDATE is a DATE: a moment without fractions of a second.
		"SYSDATE":           {Postgres: "LOCALTIMESTAMP(0)", MySQL: "NOW()"},
		"CURRENT_DATE":      {Postgres: "LOCALTIMESTAMP(0)", MySQL: "NOW()"},
		"SYSTIMESTAMP":      {Postgres: "CURRENT_TIMESTAMP", MySQL: "NOW(6)"},
		"CURRENT_TIMESTAMP": {Postgres: "CURRENT_TIMESTAMP", MySQL: "NOW(6)"},
		"LOCALTIMESTAMP":    {Postgres: "LOCALTIMESTAMP", MySQL: "NOW(6)"},
	},
	Postgres: {
		"CURRENT_DATE":      {MySQL: "CURRENT_DATE"},
		"CURRENT_TIMESTAMP": {MySQL: "NOW(6)"},
		"LOCALTIMESTAMP":    {MySQL: "NOW(6)"},
	},
}

// word translates a keyword, or hands over to chain for a name or a call.
func (s *seq) word(n *node) error {
	t := s.t
	up := strings.ToUpper(n.tok.text)
	next := s.peek(1)
	called := next != nil && next.isGroup('(')
	keyword := func(text string) error {
		s.push(pKeyword, text)
		s.i++
		return nil
	}
	leaveFrom := func() { s.inFrom, s.expectRel = false, false }

	if (up == "DATE" || up == "TIMESTAMP") && next != nil && next.isString() {
		// A typed literal in the ISO form reads the same everywhere.
		form := dateRe
		if up == "TIMESTAMP" {
			form = timestampRe
		}
		if !form.MatchString(next.tok.text) {
			return unsupported("it has a %s literal that is not in the ISO form", up)
		}
		s.out = append(s.out, t.date(up+" '"+next.tok.text+"'"))
		s.i += 2
		return nil
	}
	if called && callable[up] {
		return s.chain()
	}
	switch up {
	case "SELECT":
		s.sawSelect = true
		leaveFrom()
		return keyword(up)
	case "DISTINCT":
		if p := s.last(); p != nil && p.kind == pKeyword && (p.text == "IS" || p.text == "NOT") {
			return unsupported("it uses IS DISTINCT FROM")
		}
		return keyword(up)
	case "FROM":
		if !s.sawSelect {
			return unsupported("it uses FROM inside an expression this translator does not know")
		}
		if t.o.From == Oracle {
			if skip, err := s.dual(); skip > 0 || err != nil {
				s.i += skip
				return err
			}
		}
		s.inFrom, s.expectRel = true, true
		return keyword(up)
	case "WHERE", "GROUP", "HAVING", "ORDER":
		leaveFrom()
		return keyword(up)
	case "UNION":
		leaveFrom()
		s.sawSelect = false
		return keyword(up)
	case "INTERSECT", "EXCEPT", "MINUS":
		// MySQL only has these from 8.0.31 on.
		if t.o.From != Oracle || t.o.To != Postgres {
			return unsupported("it uses %s", up)
		}
		leaveFrom()
		s.sawSelect = false
		if up == "MINUS" {
			up = "EXCEPT"
		}
		return keyword(up)
	case "LIMIT", "OFFSET":
		// Which rows a limit keeps depends on their order, and the
		// databases do not agree on where NULL sorts: first in MySQL,
		// last in PostgreSQL and Oracle.
		if t.o.From == Oracle && !(next != nil && next.isNumber()) {
			break
		}
		return unsupported("it uses %s, and which rows that keeps depends on how NULL is sorted, which the databases do differently", up)
	case "JOIN":
		if !s.inFrom {
			return unsupported("it uses JOIN outside a FROM clause")
		}
		s.expectRel = true
		return keyword(up)
	case "FULL":
		if t.o.To == MySQL {
			return unsupported("it uses a FULL OUTER JOIN, which MySQL does not have")
		}
		return keyword(up)
	case "ON":
		s.expectRel = false
		return keyword(up)
	case "USING":
		return s.using(next)
	case "LIKE":
		// Oracle has no escape character unless the pattern names one; the
		// other two take a backslash for it.
		after := s.peek(2)
		if t.o.From == Oracle && next != nil && next.isString() && strings.Contains(next.tok.text, `\`) && (after == nil || !after.isWord("ESCAPE")) {
			return unsupported("it has a LIKE pattern with a backslash, which is an escape character on the target and not in Oracle")
		}
		return keyword(up)
	case "END":
		return s.endCase()
	case "NULL":
		s.out = append(s.out, piece{kind: pPrimary, text: up, lit: nullLiteral})
		s.i++
		return nil
	case "TRUE", "FALSE":
		s.push(pPrimary, up)
		s.i++
		return nil
	case "NULLS":
		// Both sort NULL last by default, and both take the same words.
		if t.o.From != Oracle || t.o.To != Postgres || next == nil || !(next.isWord("FIRST") || next.isWord("LAST")) {
			return unsupported("it uses NULLS FIRST or NULLS LAST")
		}
		s.push(pKeyword, "NULLS "+strings.ToUpper(next.tok.text))
		s.i += 2
		return nil
	case "WITH":
		// WITH READ ONLY says nothing about what the view returns.
		after := s.peek(2)
		if s.top && t.o.From == Oracle && next != nil && next.isWord("READ") && after != nil && after.isWord("ONLY") && s.i+3 == len(s.nodes) {
			s.i += 3
			return nil
		}
	}
	if plainKeywords[up] {
		return keyword(up)
	}
	if what, ok := t.refusal(up, called); ok {
		return unsupported("it uses %s", what)
	}
	// Words that only mean something to Oracle in front of what follows.
	if t.o.From == Oracle && next != nil && ((up == "FETCH" && (next.isWord("FIRST") || next.isWord("NEXT"))) || (up == "INTERVAL" && next.isString())) {
		return unsupported("it uses %s", up)
	}
	if text, ok := now[t.o.From][up][t.o.To]; ok && !called {
		s.out = append(s.out, t.date(text))
		s.i++
		return nil
	}
	if t.o.From == MySQL && !called {
		// MySQL writes every name of a stored definition in backticks, so
		// a bare word is a keyword this translator has no rule for.
		return unsupported("it uses %s", up)
	}
	return s.chain()
}

// dual deals with Oracle's one-row table. PostgreSQL has none and needs
// none: a SELECT without FROM returns one row. MySQL has it under the
// same name. It returns how many tokens it took care of.
func (s *seq) dual() (int, error) {
	next, after := s.peek(1), s.peek(2)
	n := 2
	if next != nil && next.isWord("SYS") && after != nil && after.isOp(".") {
		next, after, n = s.peek(3), s.peek(4), 4
	}
	if next == nil || !next.isWord("DUAL") {
		return 0, nil
	}
	// Anything but the end of the table list after it (an alias, a
	// join) makes DUAL a table like any other, which it is not here.
	if after != nil && !(after.isWord("WHERE") || after.isWord("UNION") || after.isWord("GROUP") || after.isWord("ORDER") ||
		after.isWord("MINUS") || after.isWord("INTERSECT") || after.isWord("HAVING")) {
		return 0, unsupported("it joins to DUAL")
	}
	if s.t.o.To == MySQL {
		s.push(pKeyword, "FROM DUAL")
	}
	s.inFrom, s.expectRel = false, false
	return n, nil
}

// using translates the column list of JOIN ... USING.
func (s *seq) using(list *node) error {
	if list == nil || !list.isGroup('(') {
		return unsupported("it uses USING in a form this translator does not know")
	}
	var cols []string
	for i, item := range list.items {
		switch {
		case i%2 == 1 && item.isOp(","):
		case i%2 == 0 && item.isName():
			cols = append(cols, s.t.ident(s.t.canon(item.tok)))
		default:
			return unsupported("it uses USING in a form this translator does not know")
		}
	}
	s.push(pKeyword, "USING ("+strings.Join(cols, ", ")+")")
	s.expectRel = false
	s.i += 2
	return nil
}

// endCase closes a CASE and makes one operand of the whole expression.
func (s *seq) endCase() error {
	for j := len(s.out) - 1; j >= 0; j-- {
		if s.out[j].kind == pKeyword && s.out[j].text == "CASE" {
			inner, err := s.t.finish(append(s.out[j:len(s.out):len(s.out)], piece{kind: pKeyword, text: "END"}))
			if err != nil {
				return err
			}
			whole := piece{kind: pPrimary, text: render(inner)}
			for _, p := range inner {
				whole.temporal = whole.temporal || p.temporal
			}
			s.out = append(s.out[:j], whole)
			s.i++
			return nil
		}
	}
	return unsupported("it uses END without a CASE")
}

// chain translates a name with its qualifiers: a table in a FROM clause,
// a column anywhere else, or the name of a function when an argument
// list follows.
func (s *seq) chain() error {
	t := s.t
	var parts []string
	star := false
	for {
		parts = append(parts, t.canon(s.nodes[s.i].tok))
		s.i++
		dot, after := s.peek(0), s.peek(1)
		if dot == nil || !dot.isOp(".") || after == nil {
			break
		}
		if after.isOp("*") {
			star = true
			s.i += 2
			break
		}
		if !after.isName() {
			break
		}
		s.i++
	}
	if args := s.peek(0); args != nil && args.isGroup('(') && !star {
		if len(args.items) == 1 && args.items[0].isOp("+") {
			return unsupported("it uses Oracle's (+) outer join")
		}
		name := parts[len(parts)-1]
		// PostgreSQL writes its own functions without a schema, or with
		// pg_catalog; any other qualifier is somebody's own function.
		if len(parts) > 2 || (len(parts) == 2 && !(t.o.From == Postgres && parts[0] == "pg_catalog")) {
			return unsupported("it calls %s, a function defined in the source database", strings.Join(parts, "."))
		}
		dates := t.dates
		text, err := t.call(strings.ToUpper(name), args.items)
		if err != nil {
			return err
		}
		upper := strings.ToUpper(name)
		switch {
		case yieldsDate[upper]:
			s.out = append(s.out, t.date(text))
		case yieldsNumber[upper]:
			s.push(pPrimary, text)
		default:
			// Whatever is made of a date may be a date.
			s.out = append(s.out, piece{kind: pPrimary, text: text, temporal: t.dates > dates})
		}
		s.i++
		return nil
	}
	if s.expectRel {
		return s.relation(parts, star)
	}

	switch n := len(parts); {
	case star && n == 1:
		s.push(pPrimary, t.ident(parts[0])+".*")
	case star && n == 2, !star && n == 3:
		// The schema goes: on the target the table is known by its own
		// name or by the alias the translation gave it. MySQL also puts
		// the database in front of an alias, when the alias is for a view.
		if _, ok := t.o.Relation(parts[0], parts[1]); !ok && !strings.EqualFold(parts[0], t.o.Schema) {
			return unsupported("it reads %s.%s, which is not part of the migration", parts[0], parts[1])
		}
		if star {
			s.push(pPrimary, t.ident(parts[1])+".*")
		} else {
			s.out = append(s.out, t.column(t.ident(parts[1])+"."+t.ident(parts[2]), parts[2]))
		}
	case !star && n == 1:
		s.out = append(s.out, t.column(t.ident(parts[0]), parts[0]))
	case !star && n == 2:
		s.out = append(s.out, t.column(t.ident(parts[0])+"."+t.ident(parts[1]), parts[1]))
	default:
		return unsupported("it has a name with more parts (%s) than this translator can place", strings.Join(parts, "."))
	}
	return nil
}

// relation translates the name of a table or view in a FROM clause.
func (s *seq) relation(parts []string, star bool) error {
	t := s.t
	if star || len(parts) > 2 {
		return unsupported("it reads %s, which is not a table or view of the schemas being migrated", strings.Join(parts, "."))
	}
	ref := Ref{Schema: t.o.Schema, Name: parts[0]}
	if len(parts) == 2 {
		ref = Ref{Schema: parts[0], Name: parts[1]}
	}
	rel, ok := t.o.Relation(ref.Schema, ref.Name)
	if !ok {
		return unsupported("it reads %s, which is not part of the migration", ref)
	}
	if !t.seen[ref] {
		t.seen[ref] = true
		t.reads = append(t.reads, ref)
	}
	text := rel.Quoted
	if rel.Bare != t.o.Ident(ref.Name) && !s.aliased() {
		text += " AS " + t.ident(ref.Name)
	}
	s.push(pPrimary, text)
	s.expectRel = false
	return nil
}

// aliased reports whether the table just read is given an alias.
func (s *seq) aliased() bool {
	n := s.peek(0)
	if n == nil || !n.isName() {
		return false
	}
	if n.tok.kind == tQuoted || n.isWord("AS") {
		return true
	}
	up := strings.ToUpper(n.tok.text)
	switch up {
	case "WHERE", "GROUP", "HAVING", "ORDER", "UNION", "INTERSECT", "EXCEPT", "MINUS", "LIMIT", "OFFSET", "JOIN", "ON", "USING", "FULL", "WITH":
		return false
	}
	_, no := s.t.refusal(up, false)
	return !plainKeywords[up] && !no
}

// operator translates an operator or punctuation mark.
func (s *seq) operator(n *node) error {
	t := s.t
	op := n.tok.text
	next, after := s.peek(1), s.peek(2)
	switch op {
	case ",":
		s.push(pComma, ",")
		if s.inFrom {
			s.expectRel = true
		}
	case "=", "<>":
		// PostgreSQL stores x IN ('a', 'b') as x = ANY (ARRAY['a', 'b']).
		word := map[string]string{"=": "ANY", "<>": "ALL"}[op]
		if t.o.From == Postgres && next != nil && next.isWord(word) && after != nil && after.isGroup('(') {
			return s.inList(op == "<>", after)
		}
		s.push(pOp, op)
	case "+", "-", "*", "/", "<", ">", "<=", ">=", "!=":
		s.push(pOp, op)
	case "%":
		if t.o.From == Oracle {
			return unsupported("it uses the operator %%")
		}
		s.push(pOp, op)
	case "||":
		// In MySQL this is OR unless a server setting says otherwise.
		if t.o.From == MySQL {
			return unsupported("it uses ||, whose meaning in MySQL depends on the server's SQL mode")
		}
		s.push(pOp, op)
	case "<=>":
		if t.o.To != Postgres {
			return unsupported("it uses the operator <=>")
		}
		s.push(pKeyword, "IS NOT DISTINCT FROM")
	case "~~", "!~~":
		// How PostgreSQL stores LIKE and NOT LIKE.
		if t.o.From != Postgres {
			return unsupported("it uses the operator %s", op)
		}
		s.push(pKeyword, map[string]string{"~~": "LIKE", "!~~": "NOT LIKE"}[op])
	case "::":
		if t.o.From != Postgres {
			return unsupported("it uses the operator ::")
		}
		return s.cast()
	default:
		return unsupported("it uses the operator %s", op)
	}
	s.i++
	return nil
}

// inList turns PostgreSQL's = ANY (ARRAY[...]) back into IN (...), and
// <> ALL (ARRAY[...]) into NOT IN (...). list is the bracket after ANY.
func (s *seq) inList(negate bool, list *node) error {
	items := list.items
	// A list of varchar values is wrapped once more: (ARRAY[...])::text[].
	if len(items) >= 2 && items[0].isGroup('(') && items[1].isOp("::") {
		items = items[0].items
	}
	if len(items) < 2 || !items[0].isWord("ARRAY") || !items[1].isGroup('[') || (len(items) > 2 && !items[2].isOp("::")) {
		return unsupported("it compares with ANY or ALL of something other than a list of values")
	}
	values, err := s.t.expr(items[1].items)
	if err != nil {
		return err
	}
	word := "IN"
	if negate {
		word = "NOT IN"
	}
	s.push(pKeyword, word)
	s.push(pPrimary, "("+values+")")
	s.i += 3
	return nil
}

// yieldsDate are the functions whose result is a date or time, by their
// name in the source, and yieldsNumber those that make a number even of a
// date.
var (
	yieldsDate = map[string]bool{"NOW": true, "CURRENT_TIMESTAMP": true, "LOCALTIMESTAMP": true, "TRANSACTION_TIMESTAMP": true,
		"CURDATE": true, "CURRENT_DATE": true, "DATE": true}
	yieldsNumber = map[string]bool{"COUNT": true, "EXTRACT": true, "DATE_PART": true, "YEAR": true, "MONTH": true, "DAY": true,
		"DAYOFMONTH": true, "LENGTH": true, "CHAR_LENGTH": true, "CHARACTER_LENGTH": true, "OCTET_LENGTH": true, "SIGN": true}
)

// finish applies what needs whole operands, once a level has been read:
// the check on arithmetic with dates, Oracle's division for PostgreSQL,
// and string concatenation.
func (t *translator) finish(p []piece) ([]piece, error) {
	isOp := func(i int, ops ...string) bool {
		if i < 0 || i >= len(p) || p[i].kind != pOp {
			return false
		}
		for _, o := range ops {
			if p[i].text == o {
				return true
			}
		}
		return false
	}
	if t.o.To == MySQL {
		// PostgreSQL and Oracle add days to a date with + and take two
		// dates apart with -. MySQL accepts both and computes with the
		// digits of the date instead. The other targets refuse what they
		// do not understand, so only this one needs watching.
		for i := range p {
			if !isOp(i, "+", "-") {
				continue
			}
			for _, j := range []int{i - 1, i + 1} {
				if j >= 0 && j < len(p) && p[j].kind == pPrimary && p[j].temporal {
					return nil, unsupported("it adds to or subtracts from a date, which MySQL does not do with + and -")
				}
			}
		}
	}
	if t.o.From == Oracle && t.o.To == Postgres {
		// Oracle divides exactly whatever the operands are. PostgreSQL
		// drops the remainder when both are integers, which the NUMBER
		// columns of the source often become; a numeric divisor makes it
		// divide exactly too.
		for i := range p {
			if !isOp(i, "/") {
				continue
			}
			j := i + 1
			for isOp(j, "+", "-") {
				j++
			}
			if j >= len(p) || p[j].kind != pPrimary {
				return nil, unsupported("it divides by an expression this translator cannot follow")
			}
			p[j] = piece{kind: pPrimary, text: "(" + p[j].text + ")::numeric"}
		}
	}

	var out []piece
	for i := 0; i < len(p); i++ {
		if !isOp(i+1, "||") {
			if isOp(i, "||") {
				return nil, unsupported("it uses || on an expression this translator cannot follow")
			}
			out = append(out, p[i])
			continue
		}
		// p[i] starts a run of operands joined by ||.
		start := i
		var operands []string
		for {
			if p[i].kind != pPrimary {
				return nil, unsupported("it uses || on an expression this translator cannot follow")
			}
			operands = append(operands, p[i].text)
			if !isOp(i+1, "||") {
				break
			}
			if i += 2; i >= len(p) {
				return nil, unsupported("it uses || on an expression this translator cannot follow")
			}
		}
		// The dialects disagree on whether || binds tighter than the
		// arithmetic next to it.
		arithmetic := []string{"+", "-", "*", "/", "%"}
		if isOp(start-1, arithmetic...) || isOp(i+1, arithmetic...) {
			return nil, unsupported("it mixes || with arithmetic without brackets")
		}
		list := strings.Join(operands, ", ")
		var text string
		switch {
		case t.o.From == Postgres:
			// NULL if any operand is NULL, on both sides.
			text = "CONCAT(" + list + ")"
		case t.o.To == Postgres:
			// Oracle's || takes NULL for an empty string, and an empty
			// result is NULL again.
			text = "NULLIF(concat(" + list + "), '')"
		default:
			text = "NULLIF(CONCAT_WS('', " + list + "), '')"
		}
		out = append(out, piece{kind: pPrimary, text: text})
	}
	return out, nil
}
