package codesql

import (
	"fmt"
	"strings"
)

// Dialect is one of the three languages routines are read from or
// written in.
type Dialect int

const (
	Oracle Dialect = iota + 1
	Postgres
	MySQL
)

func (d Dialect) String() string {
	switch d {
	case Oracle:
		return "Oracle"
	case Postgres:
		return "PostgreSQL"
	case MySQL:
		return "MySQL"
	}
	return "an unknown database"
}

type tokenKind int

const (
	tWord    tokenKind = iota // a keyword, a function name or an unquoted identifier
	tQuoted                   // a quoted identifier; text is the name itself
	tString                   // a string literal; text is its value
	tNumber                   // a numeric literal as written
	tOp                       // an operator or a punctuation mark
	tBind                     // :name in Oracle (the row names of a trigger); text is the name
	tParam                    // $1 in PostgreSQL; text is the number
	tUserVar                  // @name in MySQL; text is the name
	tEOF
)

// token is one lexical unit of a routine. Comments never become tokens.
type token struct {
	kind tokenKind
	text string
	up   string // the word in upper case, for comparing keywords; empty for other kinds
	line int
}

func (t token) is(word string) bool { return t.kind == tWord && t.up == word }
func (t token) op(text string) bool { return t.kind == tOp && t.text == text }

// describe names a token in a message.
func (t token) describe() string {
	switch t.kind {
	case tEOF:
		return "the end of the text"
	case tString:
		return "a string literal"
	case tQuoted:
		return `"` + t.text + `"`
	case tBind:
		return ":" + t.text
	case tParam:
		return "$" + t.text
	case tUserVar:
		return "@" + t.text
	}
	return t.text
}

// lexError is a place the text could not be split into tokens.
type lexError struct {
	line int
	msg  string
}

func (e *lexError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// operators are the marks longer than one character, longest first, so
// that "<=>" is not read as "<=" and ">".
var operators = []string{"<=>", "->>", "::", "<=", ">=", "<>", "!=", "^=", "~=", "||", "&&", ":=", "=>", "..", "<<", ">>", "->", "**"}

const singleOperators = "()[],.;+-*/%=<>!~^&|@?:{}"

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isWordStart reports whether c can begin a word. Bytes above ASCII are
// part of a multi-byte letter.
func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordPart(c byte) bool { return isWordStart(c) || isDigit(c) || c == '$' || c == '#' }

// lex splits the text of a routine into tokens. What is inside a string
// literal, a quoted identifier or a comment is never looked at as code,
// which is the reason every rewrite works on tokens: turning NVL into
// COALESCE must not touch the text 'NVL' or a column named "NVL".
func lex(d Dialect, s string) ([]token, error) {
	var out []token
	line := 1
	add := func(k tokenKind, text string, at int) {
		t := token{kind: k, text: text, line: at}
		if k == tWord {
			t.up = strings.ToUpper(text)
		}
		out = append(out, t)
	}
	for i := 0; i < len(s); {
		c := s[i]
		start := line
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(s[i:], "--") && (d != MySQL || i+2 >= len(s) || s[i+2] <= ' '), c == '#' && d == MySQL:
			// MySQL takes "--" for a comment only when white space follows;
			// "a--b" there subtracts a negative.
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, &lexError{line, "a comment is never closed"}
			}
			if d == MySQL && strings.HasPrefix(s[i+2:], "!") {
				// MySQL runs what is inside such a comment.
				return nil, &lexError{line, "a version-specific comment (/*! ... */) holds code"}
			}
			line += strings.Count(s[i:i+end+4], "\n")
			i += end + 4
		case c == '\'' || (c == '"' && d == MySQL):
			text, next, err := readString(s, i, d == MySQL, true)
			if err != nil {
				return nil, &lexError{line, err.Error()}
			}
			line += strings.Count(s[i:next], "\n")
			add(tString, text, start)
			i = next
		case c == '"' || (c == '`' && d == MySQL):
			end := i + 1
			var name strings.Builder
			for ; ; end++ {
				if end >= len(s) {
					return nil, &lexError{line, "a quoted name is never closed"}
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
			add(tQuoted, name.String(), start)
			i = end + 1
		case c == '$' && d == Postgres:
			// Either a positional parameter or a dollar-quoted string.
			if i+1 < len(s) && isDigit(s[i+1]) {
				end := i + 1
				for end < len(s) && isDigit(s[end]) {
					end++
				}
				add(tParam, s[i+1:end], start)
				i = end
				continue
			}
			close := strings.IndexByte(s[i+1:], '$')
			if close < 0 {
				return nil, &lexError{line, "a dollar sign does not start a quoted string"}
			}
			tag := s[i : i+close+2]
			if strings.ContainsAny(tag, " \t\n()") {
				return nil, &lexError{line, "a dollar sign does not start a quoted string"}
			}
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				return nil, &lexError{line, "a dollar-quoted string is never closed"}
			}
			body := s[i+len(tag) : i+len(tag)+end]
			line += strings.Count(body, "\n")
			add(tString, body, start)
			i += 2*len(tag) + end
		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			end := i
			for end < len(s) && isDigit(s[end]) {
				end++
			}
			// "1..10" is a range in a FOR loop, not the number "1." and ".10".
			if end < len(s) && s[end] == '.' && !(end+1 < len(s) && s[end+1] == '.') {
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
			if end < len(s) && isWordPart(s[end]) {
				return nil, &lexError{line, fmt.Sprintf("a number is written in a form (%s...) that is specific to %s", s[i:end+1], d)}
			}
			add(tNumber, s[i:end], start)
			i = end
		case isWordStart(c):
			end := i
			for end < len(s) && isWordPart(s[end]) {
				end++
			}
			w := s[i:end]
			if lower := strings.ToLower(w); end < len(s) && s[end] == '\'' && !typedLiteral[lower] {
				// A word right in front of a quote changes how the
				// literal is read. DATE'...' and its kind are the
				// exception: typed literals usually written with a space.
				text, next, err := prefixedString(d, lower, s, end)
				if err != nil {
					return nil, &lexError{line, err.Error()}
				}
				line += strings.Count(s[i:next], "\n")
				add(tString, text, start)
				i = next
				continue
			}
			add(tWord, w, start)
			i = end
		case c == ':' && d == Oracle && i+1 < len(s) && isWordStart(s[i+1]):
			end := i + 1
			for end < len(s) && isWordPart(s[end]) {
				end++
			}
			add(tBind, s[i+1:end], start)
			i = end
		case c == '@' && d == MySQL && i+1 < len(s) && (isWordStart(s[i+1]) || s[i+1] == '@'):
			end := i + 1
			for end < len(s) && (isWordPart(s[end]) || s[end] == '@' || s[end] == '.') {
				end++
			}
			add(tUserVar, s[i+1:end], start)
			i = end
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
				return nil, &lexError{line, fmt.Sprintf("the character %q is not part of the language", string(c))}
			}
			add(tOp, found, start)
			i += len(found)
		}
	}
	return out, nil
}

// typedLiteral lists the words that may stand directly in front of a
// quote without changing how the string is read.
var typedLiteral = map[string]bool{"date": true, "timestamp": true, "interval": true, "time": true}

// prefixedString reads a literal that has a word glued to its opening
// quote, such as E'...' in PostgreSQL or q'[...]' in Oracle. at is the
// position of the quote.
func prefixedString(d Dialect, prefix, s string, at int) (string, int, error) {
	switch {
	case d == Postgres && prefix == "e":
		return readString(s, at, true, false)
	case d == Oracle && (prefix == "q" || prefix == "nq"):
		return readAlternative(s, at)
	case d != Postgres && prefix == "n":
		// A national character literal; the text is the same.
		return readString(s, at, d == MySQL, true)
	}
	return "", 0, fmt.Errorf("a literal written as %s'...' is specific to %s", prefix, d)
}

// readString reads a quoted literal that starts at s[at] and returns its
// value and the position after it. A doubled quote stands for one;
// escapes says a backslash starts an escape sequence, as it does in MySQL
// and in PostgreSQL's E'...' form. keepLike is MySQL's rule that \% and \_
// keep their backslash, so that they still stand for themselves in a LIKE
// pattern.
func readString(s string, at int, escapes, keepLike bool) (string, int, error) {
	quote := s[at]
	var b strings.Builder
	for i := at + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == quote && i+1 < len(s) && s[i+1] == quote:
			b.WriteByte(quote)
			i++
		case c == quote:
			return b.String(), i + 1, nil
		case c == '\\' && escapes:
			i++
			if i >= len(s) {
				return "", 0, fmt.Errorf("a string literal is never closed")
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case '0':
				b.WriteByte(0)
			case 'Z':
				b.WriteByte(26)
			case '%', '_':
				if keepLike {
					b.WriteByte('\\')
				}
				b.WriteByte(s[i])
			case 'x', 'u', 'U', '1', '2', '3', '4', '5', '6', '7':
				return "", 0, fmt.Errorf("a string literal has a numeric escape sequence")
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("a string literal is never closed")
}

// readAlternative reads Oracle's q'[...]' form, in which the character
// after the quote chooses the closing mark and nothing inside is special.
func readAlternative(s string, at int) (string, int, error) {
	if at+2 >= len(s) {
		return "", 0, fmt.Errorf("a string literal is never closed")
	}
	open := s[at+1]
	closing := open
	switch open {
	case '[':
		closing = ']'
	case '(':
		closing = ')'
	case '{':
		closing = '}'
	case '<':
		closing = '>'
	}
	end := strings.Index(s[at+2:], string(closing)+"'")
	if end < 0 {
		return "", 0, fmt.Errorf("a string literal is never closed")
	}
	return s[at+2 : at+2+end], at + 2 + end + 2, nil
}
