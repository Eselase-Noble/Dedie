package orapg

import (
	"encoding/hex"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// numberFormat makes TO_CHAR print a number in full, with a point as the
// decimal separator whatever the session's territory.
const numberFormat = `'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''`

// selectExpr is how a column is read: as text in a fixed format, so that
// a value looks the same whatever the session settings, and is exact
// where a native number or timestamp would be rounded on the way.
func selectExpr(c Column) string {
	col := oracle.Quote(c.Name)
	switch c.Kind {
	case kNumber:
		return "TO_CHAR(" + col + ", " + numberFormat + ")"
	case kDate:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS')"
	case kTimestamp:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS.FF9')"
	case kTimestampTZ:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')"
	case kTimestampLTZ:
		// Stored normalised to the database time zone; read as UTC.
		return "NVL2(" + col + ", TO_CHAR(SYS_EXTRACT_UTC(" + col + "), 'YYYY-MM-DD HH24:MI:SS.FF9') || '+00', NULL)"
	case kIntervalYM, kIntervalDS, kBool:
		return "TO_CHAR(" + col + ")"
	case kRowid:
		return "ROWIDTOCHAR(" + col + ")"
	case kXML:
		return "XMLSERIALIZE(CONTENT " + col + " AS CLOB)"
	case kJSON:
		return "JSON_SERIALIZE(" + col + " RETURNING CLOB)"
	}
	return col
}

// selectList is the column list of a query that reads a table's rows.
func selectList(t *Table) string {
	exprs := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		exprs[i] = selectExpr(c)
	}
	return strings.Join(exprs, ", ")
}

// interval rewrites Oracle's interval text as ISO 8601, in which a sign
// belongs to one field. PostgreSQL would read the leading sign of
// "-01 02:03:04" as applying to the days alone.
func interval(kind valueKind, v string) string {
	sign := ""
	if strings.HasPrefix(v, "-") {
		sign = "-"
	}
	v = strings.TrimLeft(v, "+-")
	if kind == kIntervalYM {
		var y, m int
		if _, err := fmt.Sscanf(v, "%d-%d", &y, &m); err != nil {
			return v
		}
		return fmt.Sprintf("P%s%dY%s%dM", sign, y, sign, m)
	}
	day, clock, ok := strings.Cut(v, " ")
	parts := strings.Split(clock, ":")
	if !ok || len(parts) != 3 {
		return v
	}
	trim := func(s string) string {
		s = strings.TrimLeft(s, "0")
		if s == "" || s[0] == '.' {
			s = "0" + s
		}
		return s
	}
	return fmt.Sprintf("P%s%sDT%s%sH%s%sM%s%sS", sign, trim(day), sign, trim(parts[0]), sign, trim(parts[1]), sign, trim(parts[2]))
}

// pgText converts a value read with selectExpr into the text PostgreSQL
// parses for the target column. The second result is false for NULL.
func pgText(c Column, v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case nil:
		return "", false
	case []byte:
		if c.Kind == kBinary {
			return `\x` + hex.EncodeToString(x), true
		}
		s = string(x)
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	switch c.Kind {
	case kIntervalYM, kIntervalDS:
		return interval(c.Kind, s), true
	case kText, kXML, kJSON:
		// PostgreSQL text cannot contain the NUL character.
		if strings.IndexByte(s, 0) >= 0 {
			s = strings.ReplaceAll(s, "\x00", "")
		}
	}
	return s, true
}

// copyEscaper escapes a value for COPY's text format.
var copyEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func copyField(c Column, v any) string {
	s, ok := pgText(c, v)
	if !ok {
		return `\N`
	}
	return copyEscaper.Replace(s)
}
