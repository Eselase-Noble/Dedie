package oramy

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dedie/engine/migrate/orasrc"
)

// zonedLayout is the form orasrc.SelectExpr reads a TIMESTAMP WITH TIME
// ZONE in, and utcLayout the form it is stored in.
const (
	zonedLayout = "2006-01-02 15:04:05.000000000 -07:00"
	utcLayout   = "2006-01-02 15:04:05.000000000"
)

// utcText moves a timestamp with a zone to UTC and drops the zone, which
// a DATETIME cannot hold. Text it cannot read is passed on as it is, for
// MySQL to refuse.
func utcText(s string) string {
	t, err := time.Parse(zonedLayout, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(utcLayout)
}

// isoInterval is orasrc.ISOInterval without the zeros Oracle pads the
// seconds with: the text is what is stored, and P1DT2H3M4.5S reads
// better than P1DT2H3M4.500000S.
func isoInterval(kind orasrc.Kind, v string) string {
	s := orasrc.ISOInterval(kind, v)
	if body, ok := strings.CutSuffix(s, "S"); ok && strings.Contains(body[strings.LastIndexByte(body, 'M')+1:], ".") {
		s = strings.TrimSuffix(strings.TrimRight(body, "0"), ".") + "S"
	}
	return s
}

// myValue converts a value read with orasrc.SelectExpr into what the
// MySQL driver sends for the target column: nil for NULL, bytes for a
// binary column, and otherwise text MySQL parses for the column's type.
func myValue(c Column, v any) any {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		if c.Kind == orasrc.KindBinary {
			return x
		}
		// Everything else must go out as text: MySQL refuses to read
		// JSON, for one, from a string it is told is binary.
		s = string(x)
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	switch c.Kind {
	case orasrc.KindBinary:
		return []byte(s)
	case orasrc.KindIntervalYM, orasrc.KindIntervalDS:
		return isoInterval(c.Kind, s)
	case orasrc.KindTimestampTZ:
		return utcText(s)
	case orasrc.KindTimestampLTZ:
		// Read in UTC already, and marked so.
		return strings.TrimSuffix(s, "+00")
	case orasrc.KindBool:
		if strings.EqualFold(s, "TRUE") {
			return "1"
		}
		return "0"
	case orasrc.KindText:
		// MySQL keeps a CHAR without its padding and, with a collation
		// that counts trailing blanks, would not find 'A' when asked for
		// 'A  '. Dropping the padding here gives values and keys one form.
		if strings.HasPrefix(c.Type.DDL, "char(") {
			return strings.TrimRight(s, " ")
		}
	}
	return s
}

// myKey converts one column of a key read from the redo stream. It is in
// the form values are read in, except that RAW comes as hexadecimal
// digits.
func myKey(c Column, k string) any {
	if c.Kind == orasrc.KindBinary {
		if b, err := hex.DecodeString(k); err == nil {
			return b
		}
	}
	return myValue(c, k)
}

// keyBind is the placeholder a key column's value is bound through. Keys
// are bound as text, and MySQL compares a number or a date with text by
// turning both into floating point, which is neither exact nor able to
// use the index. Converting the text to the column's own type first makes
// the comparison the one the column's index is built for.
func keyBind(c Column) string {
	switch {
	case c.Type.Approx:
		return "(? + 0E0)"
	case c.Type.integer() || c.Type.DDL == "tinyint(1)":
		return "CAST(? AS SIGNED)"
	case strings.HasPrefix(c.Type.DDL, "decimal") || strings.HasPrefix(c.Type.DDL, "datetime"):
		return "CAST(? AS " + c.Type.DDL + ")"
	}
	return "?"
}
