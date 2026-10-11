package mypg

import (
	"encoding/hex"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/charmap"

	"dedie/engine/migrate/mymy"
)

var textTypes = map[string]bool{
	"char": true, "varchar": true, "tinytext": true, "text": true, "mediumtext": true, "longtext": true,
}

func isTemporal(dataType string) bool {
	return dataType == "date" || dataType == "datetime" || dataType == "timestamp"
}

// temporal converts a MySQL date or time value. MySQL's zero dates do not
// exist in PostgreSQL's calendar and become NULL; a TIMESTAMP is read in UTC
// and is marked as such.
func temporal(dataType, v string) (string, bool) {
	if strings.HasPrefix(v, "0000-00-00") {
		return "", false
	}
	if dataType == "timestamp" {
		return v + "+00", true
	}
	return v, true
}

// toUTF8 converts text stored in a MySQL character set to UTF-8 and removes
// NUL bytes, which PostgreSQL text cannot contain. MySQL's latin1 is
// Windows-1252.
func toUTF8(charset string, b []byte) string {
	var s string
	if charset == "latin1" {
		if out, err := charmap.Windows1252.NewDecoder().Bytes(b); err == nil {
			s = string(out)
		} else {
			s = string(b)
		}
	} else {
		s = string(b)
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

func bytea(b []byte) string { return `\x` + hex.EncodeToString(b) }

// streamValue converts a value decoded from a row event into the text form
// PostgreSQL parses for the target column; nil means NULL. enum holds the
// labels of an ENUM or SET column, which the binary log gives by number.
func streamValue(c mymy.Column, enum []string, v any) []byte {
	switch x := mymy.Integer(c, v).(type) {
	case nil:
		return nil
	case int64:
		if enum != nil {
			return []byte(label(c.DataType, enum, uint64(x)))
		}
		return strconv.AppendInt(nil, x, 10)
	case uint64:
		if enum != nil {
			return []byte(label(c.DataType, enum, x))
		}
		return strconv.AppendUint(nil, x, 10)
	case float32:
		return strconv.AppendFloat(nil, float64(x), 'g', -1, 32)
	case float64:
		return strconv.AppendFloat(nil, x, 'g', -1, 64)
	case string:
		switch {
		case textTypes[c.DataType]:
			// The binary log carries text as stored, in the column's character set.
			return []byte(toUTF8(c.Charset, []byte(x)))
		case isTemporal(c.DataType):
			out, ok := temporal(c.DataType, x)
			if !ok {
				return nil
			}
			return []byte(out)
		}
		return []byte(x)
	case []byte:
		switch {
		case c.DataType == "json":
			return x
		case textTypes[c.DataType]:
			return []byte(toUTF8(c.Charset, x))
		}
		return []byte(bytea(x))
	}
	return nil
}

// label turns the binary log's number for an ENUM (a 1-based index) or a SET
// (a bitmask) into the text MySQL shows for it.
func label(dataType string, values []string, n uint64) string {
	if dataType == "enum" {
		if n >= 1 && int(n) <= len(values) {
			return values[n-1]
		}
		return ""
	}
	var members []string
	for i, v := range values {
		if n&(1<<uint(i)) != 0 {
			members = append(members, v)
		}
	}
	return strings.Join(members, ",")
}

// copyValue converts a value read by the snapshot's SELECT, which arrives as
// UTF-8 text, into a field of PostgreSQL's COPY text format.
func copyValue(c mymy.Column, v []byte) string {
	if v == nil {
		return `\N`
	}
	switch {
	case c.Binary && c.DataType != "bit":
		// The backslash of the bytea hex form is itself escaped in COPY.
		return `\\x` + hex.EncodeToString(v)
	case isTemporal(c.DataType):
		out, ok := temporal(c.DataType, string(v))
		if !ok {
			return `\N`
		}
		return out
	}
	s := string(v)
	if strings.ContainsAny(s, "\\\n\r\t\x00") {
		s = copyEscaper.Replace(s)
	}
	return s
}

var copyEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\x00", "")
