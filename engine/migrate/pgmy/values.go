package pgmy

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// convert turns a value in PostgreSQL's text form into what the MySQL driver
// should send for the target column. The caller handles NULL.
func convert(c *Column, v []byte) any {
	switch c.Kind {
	case "bool":
		if len(v) > 0 && v[0] == 't' {
			return int64(1)
		}
		return int64(0)
	case "bytea":
		// PostgreSQL prints bytea as \x followed by hex.
		if len(v) >= 2 && v[0] == '\\' && v[1] == 'x' {
			if b, err := hex.DecodeString(string(v[2:])); err == nil {
				return b
			}
		}
		return append([]byte(nil), v...)
	case "timestamptz", "timetz":
		// The source session runs in UTC, so the offset is always +00.
		return stripOffset(string(v))
	case "array":
		return arrayToJSON(string(v), false)
	case "numarray":
		return arrayToJSON(string(v), true)
	}
	return string(v)
}

// offsetRe matches the UTC offset PostgreSQL appends to a time, as in +00
// or -07:30.
var offsetRe = regexp.MustCompile(`[+-][0-9]{2}(:[0-9]{2}){0,2}$`)

func stripOffset(s string) string { return offsetRe.ReplaceAllString(s, "") }

// jsonNumberRe matches what JSON accepts as a number. NaN and Infinity do
// not qualify and are kept as strings.
var jsonNumberRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// arrayToJSON converts PostgreSQL's array text form, such as {a,"b c",NULL}
// or {{1,2},{3,4}}, into a JSON array. Elements of numeric and boolean
// arrays become JSON numbers and booleans; all others become strings.
func arrayToJSON(s string, numeric bool) string {
	var out strings.Builder
	i := 0
	// A leading dimension decoration like [1:3]= is skipped.
	if eq := strings.IndexByte(s, '='); eq >= 0 && strings.HasPrefix(s, "[") {
		i = eq + 1
	}
	for i < len(s) {
		ch := s[i]
		switch {
		case ch == '{':
			out.WriteByte('[')
			i++
		case ch == '}':
			out.WriteByte(']')
			i++
		case ch == ',':
			out.WriteByte(',')
			i++
		case ch == '"':
			var elem strings.Builder
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				elem.WriteByte(s[i])
				i++
			}
			i++
			out.WriteString(strconv.Quote(elem.String()))
		default:
			start := i
			for i < len(s) && s[i] != ',' && s[i] != '}' {
				i++
			}
			elem := s[start:i]
			switch {
			case elem == "NULL":
				out.WriteString("null")
			case numeric && elem == "t":
				out.WriteString("true")
			case numeric && elem == "f":
				out.WriteString("false")
			case numeric && jsonNumberRe.MatchString(elem):
				out.WriteString(elem)
			default:
				out.WriteString(strconv.Quote(elem))
			}
		}
	}
	return out.String()
}

// copyFields splits one line of PostgreSQL's COPY text format into its
// fields and removes the escaping. A nil field is NULL.
func copyFields(line []byte, fields [][]byte) [][]byte {
	fields = fields[:0]
	start := 0
	for i := 0; i <= len(line); i++ {
		if i == len(line) || line[i] == '\t' {
			fields = append(fields, copyField(line[start:i]))
			start = i + 1
		}
	}
	return fields
}

func copyField(f []byte) []byte {
	if len(f) == 2 && f[0] == '\\' && f[1] == 'N' {
		return nil
	}
	if !containsByte(f, '\\') {
		// An empty field is an empty string, not NULL.
		if f == nil {
			return []byte{}
		}
		return f
	}
	out := make([]byte, 0, len(f))
	for i := 0; i < len(f); i++ {
		if f[i] != '\\' || i+1 == len(f) {
			out = append(out, f[i])
			continue
		}
		i++
		switch f[i] {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'v':
			out = append(out, '\v')
		default:
			out = append(out, f[i])
		}
	}
	return out
}

func containsByte(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}
