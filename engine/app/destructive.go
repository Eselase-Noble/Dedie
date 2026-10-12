package app

import (
	"encoding/json"
	"strings"
	"unicode"
)

// destructive reports whether text, about to be run with writes allowed,
// holds a statement that throws data or structure away wholesale: DROP,
// TRUNCATE, ALTER ... DROP, or a DELETE or UPDATE with no WHERE. It returns
// a few words naming the first such statement, or "" when there is none.
//
// It reads the statements a person types, not a program: comments, quoted
// text and dollar-quoted bodies are skipped, so a DROP inside a string or a
// function body is not counted. That is the right side to err on for a
// check whose purpose is to make someone look twice before running the
// obvious thing against production.
func destructive(kind, text string) string {
	if kind == "mongodb" {
		return destructiveCommand(text)
	}
	for _, stmt := range strings.Split(stripSQL(text), ";") {
		words := strings.FieldsFunc(strings.ToUpper(stmt), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		})
		if len(words) == 0 {
			continue
		}
		has := func(w string) bool {
			for _, x := range words {
				if x == w {
					return true
				}
			}
			return false
		}
		switch words[0] {
		case "DROP", "TRUNCATE":
			// "DROP TABLE" says more than "DROP"; a name that follows
			// directly, as in "TRUNCATE orders", is not part of it.
			if len(words) > 1 && objectKinds[words[1]] {
				return words[0] + " " + words[1]
			}
			return words[0]
		case "ALTER":
			if has("DROP") {
				return "ALTER ... DROP"
			}
		case "DELETE", "UPDATE":
			if !has("WHERE") {
				return words[0] + " without WHERE"
			}
		case "WITH":
			// A common table expression can end in a DELETE or UPDATE.
			// Its own WHERE clauses say nothing about that statement, so
			// the mere presence of one is enough to ask.
			for i, w := range words {
				if w == "DELETE" || (w == "UPDATE" && words[i-1] != "FOR") {
					return "WITH ... " + w
				}
			}
		}
	}
	return ""
}

// objectKinds are the words that name what a DROP or TRUNCATE acts on.
var objectKinds = map[string]bool{"TABLE": true, "SCHEMA": true, "DATABASE": true, "INDEX": true, "VIEW": true, "SEQUENCE": true,
	"FUNCTION": true, "PROCEDURE": true, "TRIGGER": true, "TYPE": true, "USER": true, "ROLE": true, "MATERIALIZED": true,
	"TABLESPACE": true, "PACKAGE": true, "EXTENSION": true, "EVENT": true}

// stripSQL returns sql with its comments, quoted strings, quoted
// identifiers and dollar-quoted bodies replaced by spaces, leaving the
// keywords and the semicolons that separate statements.
func stripSQL(sql string) string {
	var out strings.Builder
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-', c == '#':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = n
			} else {
				i += end + 4
			}
			out.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			i++
			for i < n {
				if sql[i] == '\\' && c != '`' && i+1 < n {
					i += 2
					continue
				}
				if sql[i] == c {
					i++
					// A doubled quote is the quote itself, not the end.
					if i < n && sql[i] == c {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteByte(' ')
		case c == '$':
			// $tag$ ... $tag$, PostgreSQL's quoting for bodies of code.
			end := i + 1
			for end < n && (sql[end] == '_' || unicode.IsLetter(rune(sql[end])) || unicode.IsDigit(rune(sql[end]))) {
				end++
			}
			if end < n && sql[end] == '$' {
				tag := sql[i : end+1]
				if closing := strings.Index(sql[end+1:], tag); closing >= 0 {
					i = end + 1 + closing + len(tag)
					out.WriteByte(' ')
					continue
				}
			}
			out.WriteByte(c)
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// destructiveCommand is destructive for a MongoDB command document, whose
// first field names the command.
func destructiveCommand(text string) string {
	dec := json.NewDecoder(strings.NewReader(text))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	name, _ := tok.(string)
	switch name {
	case "drop", "dropDatabase", "dropIndexes":
		return name
	case "delete":
		// Deleting with an empty filter empties the collection.
		var cmd struct {
			Deletes []struct {
				Q map[string]any `json:"q"`
			} `json:"deletes"`
		}
		if json.Unmarshal([]byte(text), &cmd) == nil {
			for _, d := range cmd.Deletes {
				if len(d.Q) == 0 {
					return "delete with an empty filter"
				}
			}
		}
	}
	return ""
}
