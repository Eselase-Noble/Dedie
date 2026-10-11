package orasrc

import (
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// numberFormat makes TO_CHAR print a number in full, with a point as the
// decimal separator whatever the session's territory.
const numberFormat = `'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''`

// SelectExpr is how a column is read: as text in a fixed format, so that
// a value looks the same whatever the session settings, and is exact
// where a native number or timestamp would be rounded on the way.
//
// The forms are: a number in full with a point as the decimal separator;
// a DATE as YYYY-MM-DD HH24:MI:SS; a timestamp the same with nine
// fractional digits, followed by " TZH:TZM" when it has a zone and by
// "+00" when the zone is the local one; an interval as Oracle prints it
// (see ISOInterval), and a BOOLEAN likewise; XML and JSON serialised.
// Text comes back as it is, and RAW and BLOB values as bytes.
func SelectExpr(c Column) string {
	col := oracle.Quote(c.Name)
	switch c.Kind {
	case KindNumber:
		number := "TO_CHAR(" + col + ", " + numberFormat + ")"
		if c.DataType == "BINARY_FLOAT" || c.DataType == "BINARY_DOUBLE" {
			// The number format cannot print the three values that are
			// not numbers and fills the field with '#' instead. Without
			// a format they come out as Inf, -Inf and Nan, which Oracle
			// and PostgreSQL both read back.
			return "CASE WHEN " + col + " IS NAN OR " + col + " IS INFINITE THEN TO_CHAR(" + col + ") ELSE " + number + " END"
		}
		return number
	case KindDate:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS')"
	case KindTimestamp:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS.FF9')"
	case KindTimestampTZ:
		return "TO_CHAR(" + col + ", 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')"
	case KindTimestampLTZ:
		// Stored normalised to the database time zone; read as UTC.
		return "NVL2(" + col + ", TO_CHAR(SYS_EXTRACT_UTC(" + col + "), 'YYYY-MM-DD HH24:MI:SS.FF9') || '+00', NULL)"
	case KindIntervalYM, KindIntervalDS, KindBool:
		return "TO_CHAR(" + col + ")"
	case KindRowid:
		return "ROWIDTOCHAR(" + col + ")"
	case KindXML:
		return "XMLSERIALIZE(CONTENT " + col + " AS CLOB)"
	case KindJSON:
		return "JSON_SERIALIZE(" + col + " RETURNING CLOB)"
	}
	return col
}

// SelectList is the column list of a query that reads a table's rows.
func SelectList(t *Table) string {
	exprs := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		exprs[i] = SelectExpr(c)
	}
	return strings.Join(exprs, ", ")
}

// ISOInterval rewrites Oracle's interval text as ISO 8601, in which a sign
// belongs to one field. PostgreSQL would read the leading sign of
// "-01 02:03:04" as applying to the days alone. It lives here because the
// text it reads is the one SelectExpr produces, and any target other than
// Oracle itself has to take that text apart.
func ISOInterval(kind Kind, v string) string {
	sign := ""
	if strings.HasPrefix(v, "-") {
		sign = "-"
	}
	v = strings.TrimLeft(v, "+-")
	if kind == KindIntervalYM {
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
