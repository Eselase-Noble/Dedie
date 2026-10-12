package viewsql

import (
	"fmt"
	"strings"
)

// Dialect names one of the SQL dialects a view can be read from or
// written in.
type Dialect int

const (
	MySQL Dialect = iota + 1
	Postgres
	Oracle
)

func (d Dialect) String() string {
	switch d {
	case MySQL:
		return "MySQL"
	case Postgres:
		return "PostgreSQL"
	case Oracle:
		return "Oracle"
	}
	return "an unknown database"
}

// Unsupported says why a definition was not translated. It is the only
// kind of error the translator returns: a definition it cannot carry over
// with the same meaning is not a failure, just a view that stays behind.
type Unsupported struct{ Reason string }

func (e *Unsupported) Error() string { return e.Reason }

func unsupported(format string, args ...any) error {
	return &Unsupported{Reason: fmt.Sprintf(format, args...)}
}

type tokenKind int

const (
	tWord   tokenKind = iota // a keyword, a function name or an unquoted identifier
	tQuoted                  // a quoted identifier; text is the name itself
	tString                  // a string literal; text is its value
	tNumber
	tOp // an operator or a punctuation mark
)

type token struct {
	kind tokenKind
	text string
}

// operators are the marks longer than one character, longest first, so
// that "<=>" is not read as "<=" and ">".
var operators = []string{"!~~*", "<=>", "~~*", "!~~", "->>", "::", "<=", ">=", "<>", "!=", "||", "~~", ":=", "->", "^=", "&&", "<<", ">>", "!~"}

const singleOperators = "()[],.;+-*/%=<>!~^&|@?:{}"

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isWordStart reports whether c can begin a word. Bytes above ASCII are
// part of a multi-byte letter.
func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordPart(c byte) bool { return isWordStart(c) || isDigit(c) || c == '$' || c == '#' }

// lex splits a statement into tokens. Comments are dropped. What is inside
// a string literal, a quoted identifier or a comment is never looked at as
// SQL, which is the whole reason the translator works on tokens: a rewrite
// of NVL must not touch the text 'NVL' or a column named "NVL".
func lex(d Dialect, s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && strings.HasPrefix(s[i:], "--"), c == '#' && d == MySQL:
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			i += end
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, unsupported("it has a comment that never ends")
			}
			if d == MySQL && strings.HasPrefix(s[i+2:], "!") {
				// MySQL runs what is inside such a comment.
				return nil, unsupported("it uses a version-specific comment (/*! ... */)")
			}
			i += end + 4
		case c == '\'' || (c == '"' && d == MySQL):
			text, next, err := readString(d, s, i, false)
			if err != nil {
				return nil, err
			}
			out, i = append(out, token{tString, text}), next
		case c == '"' || (c == '`' && d == MySQL):
			end := i + 1
			var name strings.Builder
			for ; ; end++ {
				if end >= len(s) {
					return nil, unsupported("it has a quoted name that never ends")
				}
				if s[end] == c {
					if end+1 < len(s) && s[end+1] == c {
						end++
					} else {
						break
					}
				}
				name.WriteByte(s[end])
			}
			out, i = append(out, token{tQuoted, name.String()}), end+1
		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			end := i
			for end < len(s) && isDigit(s[end]) {
				end++
			}
			if end < len(s) && s[end] == '.' {
				for end++; end < len(s) && isDigit(s[end]); end++ {
				}
			}
			if end < len(s) && (s[end] == 'e' || s[end] == 'E') {
				exp := end + 1
				if exp < len(s) && (s[exp] == '+' || s[exp] == '-') {
					exp++
				}
				if exp < len(s) && isDigit(s[exp]) {
					for end = exp; end < len(s) && isDigit(s[end]); end++ {
					}
				}
			}
			// 0x1F, 1.5f and the like mean something else in each dialect.
			if end < len(s) && isWordPart(s[end]) {
				return nil, unsupported("it has a number written in a form (%s...) that is specific to %s", s[i:end+1], d)
			}
			out, i = append(out, token{tNumber, s[i:end]}), end
		case isWordStart(c):
			end := i
			for end < len(s) && isWordPart(s[end]) {
				end++
			}
			w := s[i:end]
			if lower := strings.ToLower(w); end < len(s) && s[end] == '\'' && lower != "date" && lower != "timestamp" {
				// A word right in front of a quote changes how the
				// literal is read. DATE'...' is the exception: the same
				// typed literal that is usually written with a space.
				text, next, err := prefixedString(d, w, s, end)
				if err != nil {
					return nil, err
				}
				out, i = append(out, token{tString, text}), next
				continue
			}
			out, i = append(out, token{tWord, w}), end
		default:
			found := ""
			for _, o := range operators {
				if strings.HasPrefix(s[i:], o) {
					found = o
					break
				}
			}
			if found == "" && strings.IndexByte(singleOperators, c) >= 0 {
				found = string(c)
			}
			if found == "" {
				return nil, unsupported("it has a character (%q) this translator does not know", string(c))
			}
			out, i = append(out, token{tOp, found}), i+len(found)
		}
	}
	// One statement terminator at the very end is not part of the query.
	if n := len(out); n > 0 && out[n-1].kind == tOp && out[n-1].text == ";" {
		out = out[:n-1]
	}
	return out, nil
}

// prefixedString reads a literal that has a word glued to its opening
// quote, such as E'...' in PostgreSQL or q'[...]' in Oracle. at is the
// position of the quote.
func prefixedString(d Dialect, prefix, s string, at int) (string, int, error) {
	p := strings.ToLower(prefix)
	switch {
	case d == Postgres && p == "e":
		return readString(d, s, at, true)
	case d == Oracle && (p == "q" || p == "nq"):
		return readQuoted(s, at)
	case d != Postgres && p == "n":
		// A national character literal; the text is the same.
		return readString(d, s, at, false)
	case d == MySQL && (p == "_utf8mb4" || p == "_utf8mb3" || p == "_utf8"):
		// The definition arrives as UTF-8, so the introducer says nothing new.
		return readString(d, s, at, false)
	}
	return "", 0, unsupported("it has a literal written as %s'...', which is specific to %s", prefix, d)
}

// readString reads a string literal whose opening quote is at position at
// and returns its value. Backslash is an escape character in MySQL and in
// PostgreSQL's E'...' form, and an ordinary character everywhere else.
func readString(d Dialect, s string, at int, escapes bool) (string, int, error) {
	quote := s[at]
	escapes = escapes || d == MySQL
	var b strings.Builder
	for i := at + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == quote:
			if i+1 < len(s) && s[i+1] == quote {
				b.WriteByte(quote)
				i++
				continue
			}
			return b.String(), i + 1, nil
		case c == '\\' && escapes:
			if i+1 >= len(s) {
				return "", 0, unsupported("it has a string literal that never ends")
			}
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case '\\', '\'', '"':
				b.WriteByte(e)
			default:
				if d != MySQL {
					return "", 0, unsupported("it has a string literal with the escape \\%c", e)
				}
				switch e {
				case '0':
					b.WriteByte(0)
				case 'Z':
					b.WriteByte(26)
				case '%', '_':
					// These keep their backslash: it only matters to LIKE.
					b.WriteByte('\\')
					b.WriteByte(e)
				default:
					b.WriteByte(e)
				}
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, unsupported("it has a string literal that never ends")
}

// readQuoted reads Oracle's q'[...]' form, in which the text runs to the
// closing delimiter followed by a quote and nothing inside is escaped.
func readQuoted(s string, at int) (string, int, error) {
	if at+2 >= len(s) {
		return "", 0, unsupported("it has a string literal that never ends")
	}
	closer := s[at+1]
	switch closer {
	case '[':
		closer = ']'
	case '(':
		closer = ')'
	case '{':
		closer = '}'
	case '<':
		closer = '>'
	}
	end := strings.Index(s[at+2:], string(closer)+"'")
	if end < 0 {
		return "", 0, unsupported("it has a string literal that never ends")
	}
	return s[at+2 : at+2+end], at + 2 + end + 2, nil
}

// node is a token, or everything between a pair of brackets.
type node struct {
	tok token
	// open is '(' or '[' for a bracketed group and 0 for a single token.
	open  byte
	items []node
}

func (n *node) isOp(text string) bool  { return n.open == 0 && n.tok.kind == tOp && n.tok.text == text }
func (n *node) isString() bool         { return n.open == 0 && n.tok.kind == tString }
func (n *node) isNumber() bool         { return n.open == 0 && n.tok.kind == tNumber }
func (n *node) isName() bool           { return n.open == 0 && (n.tok.kind == tWord || n.tok.kind == tQuoted) }
func (n *node) isGroup(open byte) bool { return n.open == open }
func (n *node) isWord(upper string) bool {
	return n.open == 0 && n.tok.kind == tWord && strings.EqualFold(n.tok.text, upper)
}

// nest groups the tokens by their brackets, so that the translator can
// take a parenthesised expression, an argument list or a subquery as one
// thing.
func nest(toks []token) ([]node, error) {
	type frame struct {
		open  byte
		items []node
	}
	stack := []frame{{}}
	closers := map[string]byte{")": '(', "]": '['}
	for _, t := range toks {
		top := &stack[len(stack)-1]
		switch {
		case t.kind == tOp && (t.text == "(" || t.text == "["):
			stack = append(stack, frame{open: t.text[0]})
		case t.kind == tOp && closers[t.text] != 0:
			if len(stack) == 1 || top.open != closers[t.text] {
				return nil, unsupported("its brackets do not match")
			}
			group := node{open: top.open, items: top.items}
			stack = stack[:len(stack)-1]
			stack[len(stack)-1].items = append(stack[len(stack)-1].items, group)
		default:
			top.items = append(top.items, node{tok: t})
		}
	}
	if len(stack) != 1 {
		return nil, unsupported("its brackets do not match")
	}
	return stack[0].items, nil
}
