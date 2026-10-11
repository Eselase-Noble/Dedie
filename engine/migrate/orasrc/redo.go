package orasrc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// LogMiner operation codes, as V$LOGMNR_CONTENTS numbers them.
const (
	OpInsert     = 1
	OpDelete     = 2
	OpUpdate     = 3
	OpDDL        = 5
	OpCommit     = 7
	OpLobLocator = 9
	OpRollback   = 36
)

// Redo is one reconstructed statement, reduced to its column values. A
// nil value stands for NULL.
type Redo struct {
	// Values are the columns of an INSERT, or those assigned by an UPDATE.
	Values map[string]*string
	// Where holds the columns that identify the row an UPDATE or DELETE
	// changed, as they were before it.
	Where map[string]*string
}

// Token kinds of a reconstructed statement.
const (
	tokWord   = iota // keyword, function name or bare number
	tokName          // "quoted identifier"
	tokString        // 'literal'
	tokPunct
)

type token struct {
	kind int
	text string
}

// lexer splits the statements LogMiner writes. Their grammar is small and
// fixed, which is what makes reading them back dependable: names are
// always quoted, and a value is a string literal, NULL, or a conversion
// function around a string literal.
type lexer struct {
	s   string
	pos int
	// peeked holds a token read ahead.
	peeked *token
}

func (l *lexer) next() (token, bool) {
	if l.peeked != nil {
		t := *l.peeked
		l.peeked = nil
		return t, true
	}
	for l.pos < len(l.s) && (l.s[l.pos] == ' ' || l.s[l.pos] == '\n' || l.s[l.pos] == '\t' || l.s[l.pos] == '\r') {
		l.pos++
	}
	if l.pos >= len(l.s) {
		return token{}, false
	}
	ch := l.s[l.pos]
	switch {
	case ch == '"':
		end := strings.IndexByte(l.s[l.pos+1:], '"')
		if end < 0 {
			end = len(l.s) - l.pos - 1
		}
		t := token{tokName, l.s[l.pos+1 : l.pos+1+end]}
		l.pos = min(l.pos+end+2, len(l.s))
		return t, true
	case ch == '\'':
		var b strings.Builder
		i := l.pos + 1
		for i < len(l.s) {
			if l.s[i] == '\'' {
				if i+1 < len(l.s) && l.s[i+1] == '\'' {
					b.WriteByte('\'')
					i += 2
					continue
				}
				break
			}
			b.WriteByte(l.s[i])
			i++
		}
		l.pos = min(i+1, len(l.s))
		return token{tokString, b.String()}, true
	case ch == '|' && strings.HasPrefix(l.s[l.pos:], "||"):
		l.pos += 2
		return token{tokPunct, "||"}, true
	case strings.IndexByte("(),=;", ch) >= 0:
		l.pos++
		return token{tokPunct, string(ch)}, true
	}
	start := l.pos
	for l.pos < len(l.s) && strings.IndexByte(" \n\t\r(),=;'\"|", l.s[l.pos]) < 0 {
		l.pos++
	}
	if l.pos == start {
		l.pos++
	}
	return token{tokWord, l.s[start:l.pos]}, true
}

func (l *lexer) peek() (token, bool) {
	if l.peeked == nil {
		t, ok := l.next()
		if !ok {
			return token{}, false
		}
		l.peeked = &t
	}
	return *l.peeked, true
}

// accept consumes the next token if it is the given word or punctuation.
func (l *lexer) accept(text string) bool {
	t, ok := l.peek()
	if ok && t.kind != tokString && t.kind != tokName && strings.EqualFold(t.text, text) {
		l.peeked = nil
		return true
	}
	return false
}

// unistr decodes the argument of UNISTR: \XXXX is a UTF-16 code unit and
// \\ a backslash.
func unistr(s string) string {
	var units []uint16
	var b strings.Builder
	flush := func() {
		if len(units) > 0 {
			b.WriteString(string(utf16.Decode(units)))
			units = units[:0]
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+5], 16, 16); err == nil {
				units = append(units, uint16(n))
				i += 4
				continue
			}
		}
		flush()
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\\' {
			i++
		}
		b.WriteByte(s[i])
	}
	flush()
	return b.String()
}

// value reads one value: a literal, NULL, or a function call, whose
// first string argument is the value in text form. Pieces joined with ||
// are put back together.
func (l *lexer) value() (*string, error) {
	var out *string
	for {
		t, ok := l.next()
		if !ok {
			return nil, errors.New("the statement ends where a value was expected")
		}
		var piece *string
		switch t.kind {
		case tokString:
			piece = &t.text
		case tokWord:
			if strings.EqualFold(t.text, "NULL") {
				break
			}
			if !l.accept("(") {
				// A bare number.
				piece = &t.text
				break
			}
			arg, depth := "", 1
			first := true
			for depth > 0 {
				a, ok := l.next()
				if !ok {
					return nil, errors.New("unbalanced parentheses in a value")
				}
				switch {
				case a.kind == tokPunct && a.text == "(":
					depth++
				case a.kind == tokPunct && a.text == ")":
					depth--
				case a.kind == tokString && first:
					arg, first = a.text, false
				}
			}
			if strings.EqualFold(t.text, "UNISTR") {
				arg = unistr(arg)
			}
			piece = &arg
		default:
			return nil, fmt.Errorf("unexpected %q where a value was expected", t.text)
		}
		if piece != nil {
			if out == nil {
				out = piece
			} else {
				joined := *out + *piece
				out = &joined
			}
		}
		if !l.accept("||") {
			return out, nil
		}
	}
}

// conditions reads `"A" = value and "B" IS NULL and ROWID = '...'`.
func (l *lexer) conditions(into map[string]*string) error {
	for {
		t, ok := l.next()
		if !ok {
			return nil
		}
		name, isColumn := t.text, t.kind == tokName
		switch {
		case l.accept("IS"):
			if !l.accept("NULL") {
				return errors.New("expected NULL after IS")
			}
			if isColumn {
				into[name] = nil
			}
		case l.accept("="):
			v, err := l.value()
			if err != nil {
				return err
			}
			if isColumn {
				into[name] = v
			}
		default:
			return fmt.Errorf("unexpected %q in a condition", t.text)
		}
		if !l.accept("and") {
			return nil
		}
	}
}

// skipTo moves past the next occurrence of a keyword outside any literal.
func (l *lexer) skipTo(word string) bool {
	for {
		t, ok := l.next()
		if !ok {
			return false
		}
		if t.kind == tokWord && strings.EqualFold(t.text, word) {
			return true
		}
	}
}

// ParseRedo reads the statement LogMiner reconstructed for a row change.
func ParseRedo(op int, text string) (*Redo, error) {
	l := &lexer{s: text}
	r := &Redo{Values: map[string]*string{}, Where: map[string]*string{}}
	switch op {
	case OpInsert:
		// insert into "O"."T"("A","B") values (v, v);
		if !l.skipTo("into") {
			return nil, errors.New("not an INSERT")
		}
		var cols []string
		for {
			t, ok := l.next()
			if !ok {
				return nil, errors.New("the INSERT has no column list")
			}
			if t.kind == tokPunct && t.text == "(" {
				break
			}
		}
		for {
			t, ok := l.next()
			if !ok || (t.kind == tokPunct && t.text == ")") {
				break
			}
			if t.kind == tokName {
				cols = append(cols, t.text)
			}
		}
		if !l.accept("values") || !l.accept("(") {
			return nil, errors.New("the INSERT has no VALUES list")
		}
		for i := range cols {
			v, err := l.value()
			if err != nil {
				return nil, err
			}
			r.Values[cols[i]] = v
			if i < len(cols)-1 && !l.accept(",") {
				return nil, errors.New("the INSERT has fewer values than columns")
			}
		}
	case OpUpdate:
		// update "O"."T" set "A" = v, "B" = v where ...;
		if !l.skipTo("set") {
			return nil, errors.New("not an UPDATE")
		}
		for {
			t, ok := l.next()
			if !ok || t.kind != tokName || !l.accept("=") {
				return nil, errors.New("malformed SET clause")
			}
			v, err := l.value()
			if err != nil {
				return nil, err
			}
			r.Values[t.text] = v
			if !l.accept(",") {
				break
			}
		}
		if l.accept("where") {
			if err := l.conditions(r.Where); err != nil {
				return nil, err
			}
		}
	case OpDelete, OpLobLocator:
		// delete from "O"."T" where ...;
		// ... select "C" into loc from "O"."T" where ... for update;
		if !l.skipTo("where") {
			return nil, errors.New("the statement has no WHERE clause")
		}
		if err := l.conditions(r.Where); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("operation %d is not a row change", op)
	}
	return r, nil
}

// Keys works out which rows a statement leaves changed, by primary key:
// gone is the key a row no longer has, now the key it has after the
// change. Either may be nil. An update that leaves the key alone reports
// only now.
func (r *Redo) Keys(t *Table, op int) (gone, now []string, err error) {
	pick := func(m map[string]*string) ([]string, bool) {
		key := make([]string, len(t.PK))
		for i, col := range t.PK {
			v, ok := m[t.Cols[col].Name]
			if !ok || v == nil {
				return nil, false
			}
			key[i] = *v
		}
		return key, true
	}
	missing := fmt.Errorf("a change to %s did not carry its primary key; supplemental logging of the key is not in effect for this table", t.Label())
	switch op {
	case OpInsert:
		key, ok := pick(r.Values)
		if !ok {
			return nil, nil, missing
		}
		return nil, key, nil
	case OpDelete:
		key, ok := pick(r.Where)
		if !ok {
			return nil, nil, missing
		}
		return key, nil, nil
	case OpLobLocator:
		key, ok := pick(r.Where)
		if !ok {
			return nil, nil, missing
		}
		return nil, key, nil
	}
	old, ok := pick(r.Where)
	if !ok {
		return nil, nil, missing
	}
	changed := false
	next := append([]string(nil), old...)
	for i, col := range t.PK {
		if v, set := r.Values[t.Cols[col].Name]; set && v != nil && *v != old[i] {
			next[i], changed = *v, true
		}
	}
	if changed {
		return old, next, nil
	}
	return nil, old, nil
}
