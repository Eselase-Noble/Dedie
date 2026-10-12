package codesql

import (
	"regexp"
	"strings"
)

// The current time. Each database has several clocks, and they do not
// line up exactly:
//
//   - Oracle's SYSDATE is the server's clock, read when it is evaluated.
//   - MySQL's NOW() is the time the statement that called the routine
//     began; SYSDATE() is the clock.
//   - PostgreSQL's now() is the time the transaction began;
//     statement_timestamp() is when the client's statement arrived and
//     clock_timestamp() is the clock.
//
// The translations below pick the nearest clock. All of them give the
// time in the session's time zone on the target, where Oracle gives the
// server's; the package documentation lists that among the assumptions.
const (
	pgClockSeconds     = "date_trunc('second', clock_timestamp())::timestamp(0)"
	pgStatementSeconds = "date_trunc('second', statement_timestamp())::timestamp(0)"
)

// special writes the words a language reads as values without their
// being declared anywhere: the clocks, the pseudo-columns, the variables
// a trigger or a handler is given. first is the name's first part in
// upper case.
func (g *gen) special(n *nameExpr, first string) (string, typ, bool) {
	last := strings.ToUpper(n.parts[len(n.parts)-1].name)
	if len(n.parts) > 1 {
		if g.from == Oracle && (last == "NEXTVAL" || last == "CURRVAL") {
			if g.to == MySQL {
				g.refuse(n.line, "the sequence %s: MySQL has no sequences", n.parts[len(n.parts)-2].name)
				return "NULL", tInt, true
			}
			return strings.ToLower(last) + "(" + g.stringLit(g.objectName(n.parts[:len(n.parts)-1])) + ")", tInt, true
		}
		return "", tUnknown, false
	}
	switch g.from {
	case Oracle:
		return g.specialOracle(n, first)
	case MySQL:
		switch first {
		case "CURRENT_TIMESTAMP", "LOCALTIMESTAMP", "LOCALTIME":
			return pgStatementSeconds, typ{cTime, 0}, true
		case "CURRENT_DATE":
			return "statement_timestamp()::date", tDate, true
		case "CURRENT_USER":
			g.note(n.line, "CURRENT_USER is user@host in MySQL and a role name in PostgreSQL")
			return "current_user", tStr, true
		}
	case Postgres:
		return g.specialPostgres(n, first)
	}
	return "", tUnknown, false
}

func (g *gen) specialOracle(n *nameExpr, first string) (string, typ, bool) {
	pg := g.to == Postgres
	switch first {
	case "SYSDATE", "CURRENT_DATE":
		if pg {
			return pgClockSeconds, tTime, true
		}
		return "NOW()", tTime, true
	case "SYSTIMESTAMP", "CURRENT_TIMESTAMP":
		if pg {
			return "clock_timestamp()", tTimeTZ, true
		}
		return "NOW(6)", tTime, true
	case "LOCALTIMESTAMP":
		if pg {
			return "clock_timestamp()::timestamp", tTime, true
		}
		return "NOW(6)", tTime, true
	case "USER":
		g.note(n.line, "USER is the Oracle account in upper case; the converted code gives the %s account as that database writes it", g.to)
		if pg {
			return "current_user", tStr, true
		}
		return "SUBSTRING_INDEX(USER(), '@', 1)", tStr, true
	case "SQLERRM":
		return g.errorText(n.line)
	case "SQLCODE":
		g.refuse(n.line, "SQLCODE: the error numbers of Oracle mean nothing on the target")
		return "NULL", tInt, true
	case "ROWNUM", "ROWID", "LEVEL":
		g.refuse(n.line, "the pseudo-column %s has no translation", first)
		return "NULL", tUnknown, true
	case "INSERTING", "UPDATING", "DELETING":
		if g.r.trigger == nil {
			return "", tUnknown, false
		}
		event := strings.TrimSuffix(first, "ING")
		if event == "INSERT" || event == "DELET" || event == "UPDAT" {
			event = map[string]string{"INSERT": "INSERT", "DELET": "DELETE", "UPDAT": "UPDATE"}[event]
		}
		if pg {
			return "(TG_OP = '" + event + "')", tBool, true
		}
		if g.event == event {
			return "TRUE", tBool, true
		}
		return "FALSE", tBool, true
	}
	return "", tUnknown, false
}

// errorText writes the message of the error being handled.
func (g *gen) errorText(line int) (string, typ, bool) {
	if g.handlerDepth == 0 {
		g.refuse(line, "the error message is read outside an error handler")
		return "NULL", tStr, true
	}
	g.note(line, "the text of an error message is read; %s and %s word their errors differently", g.from, g.to)
	if g.to == Postgres {
		return "SQLERRM", tStr, true
	}
	g.usesErrText = true
	return errTextVar, tStr, true
}

func (g *gen) specialPostgres(n *nameExpr, first string) (string, typ, bool) {
	h := g.r.trigger
	switch first {
	case "CURRENT_TIMESTAMP":
		g.note(n.line, "the current time with its zone becomes UTC_TIMESTAMP(6): the data migration stores such values in UTC, so compare it only with columns that were timestamps with a time zone")
		return "UTC_TIMESTAMP(6)", tTimeTZ, true
	case "LOCALTIMESTAMP":
		return "NOW(6)", tTime, true
	case "CURRENT_DATE":
		return "CURDATE()", tDate, true
	case "CURRENT_USER", "SESSION_USER", "USER":
		g.note(n.line, "%s is a role name in PostgreSQL; the converted code gives the MySQL account name", first)
		return "SUBSTRING_INDEX(USER(), '@', 1)", tStr, true
	case "FOUND":
		g.usesFound = true
		return "(" + foundVar + " = 1)", tBool, true
	case "SQLERRM":
		return g.errorText(n.line)
	case "SQLSTATE":
		if g.handlerDepth == 0 {
			g.refuse(n.line, "SQLSTATE is read outside an error handler")
			return "NULL", tStr, true
		}
		g.note(n.line, "SQLSTATE is read; MySQL gives its own codes for the errors it raises itself")
		g.usesErrState = true
		return errStateVar, tStr, true
	}
	if h == nil {
		return "", tUnknown, false
	}
	switch first {
	case "TG_OP":
		return "'" + g.event + "'", tStr, true
	case "TG_TABLE_NAME", "TG_RELNAME":
		return g.stringLit(h.table[len(h.table)-1].name), tStr, true
	case "TG_NAME":
		return g.stringLit(h.name[len(h.name)-1].name), tStr, true
	case "TG_WHEN":
		return "'" + h.timing + "'", tStr, true
	case "TG_LEVEL":
		return "'ROW'", tStr, true
	case "TG_NARGS", "TG_ARGV", "TG_TABLE_SCHEMA", "TG_RELID":
		g.refuse(n.line, "%s has no translation", first)
		return "NULL", tUnknown, true
	}
	return "", tUnknown, false
}

// addition writes "+" and "-", which add days to dates in Oracle, need
// an INTERVAL in MySQL and work on dates and intervals in PostgreSQL.
func (g *gen) addition(e *binExpr) (string, typ) {
	switch g.from {
	case Oracle:
		return g.addOracle(e)
	case MySQL:
		return g.addMySQL(e)
	}
	return g.addPostgres(e)
}

// plainSum writes an addition of two numbers.
func (g *gen) plainSum(e *binExpr, l string, lt typ, r string, rt typ) (string, typ) {
	rs := r
	if b, ok := e.r.(*binExpr); ok && (b.op == "+" || b.op == "-") {
		rs = "(" + r + ")"
	} else if _, ok := e.r.(*unExpr); ok {
		rs = "(" + r + ")"
	}
	return l + " " + e.op + " " + rs, numericResult(lt, rt, false)
}

func (g *gen) addOracle(e *binExpr) (string, typ) {
	if g.to == MySQL {
		// An interval literal exists in MySQL only next to the date it
		// is added to.
		if n, unit, ok := g.intervalOf(e.r); ok {
			l, lt := g.value(e.l)
			return "(" + l + " " + e.op + " INTERVAL " + n + " " + unit + ")", lt
		}
	}
	rounded := g.rounded > 0
	// A number of days added to a date is multiplied by an interval, and
	// only whole seconds of it survive: the digits of a division such as
	// 1/24 do not matter there.
	g.rounded++
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	g.rounded--
	if !lt.temporal() && rt.temporal() && e.op == "+" {
		// A number plus a date is the date plus the number.
		e = &binExpr{op: "+", l: e.r, r: e.l, line: e.line}
		l, lt, r, rt = r, rt, l, lt
	}
	switch {
	case lt.temporal() && rt.temporal():
		if e.op != "-" {
			g.refuse(e.line, "two dates are added")
			return "NULL", tUnknown
		}
		if !rounded {
			g.note(e.line, "the difference %s is a number of days with a fraction; %s keeps a different number of digits of it than Oracle, so round the result if its last digits matter", describeExpr(e), g.to)
		}
		if g.to == Postgres {
			return "(EXTRACT(EPOCH FROM (" + l + " - " + r + ")) / 86400)", tNum
		}
		return "(TIMESTAMPDIFF(SECOND, " + r + ", " + l + ") / 86400)", tNum
	case lt.temporal() && rt.class == cInterval:
		if g.to == Postgres {
			return l + " " + e.op + " " + r, lt
		}
		if n, unit, ok := g.intervalOf(e.r); ok {
			return "(" + l + " " + e.op + " INTERVAL " + n + " " + unit + ")", lt
		}
		g.refuse(e.line, "the interval in %s has no MySQL form", describeExpr(e))
		return "NULL", lt
	case lt.temporal():
		if !rt.numeric() && rt.class != cNull {
			g.note(e.line, "%s: the type of %s is not known here; it is taken to be a number of days", describeExpr(e), describeExpr(e.r))
		}
		whole := rt.whole()
		if g.to == Postgres {
			s := l + " " + e.op + " " + wrapTerm(r, e.r) + " * INTERVAL '1 day'"
			if !whole {
				s = "(" + s + ")::timestamp(0)"
			}
			return s, tTime
		}
		if whole {
			return "(" + l + " " + e.op + " INTERVAL " + wrapTerm(r, e.r) + " DAY)", tTime
		}
		return "(" + l + " " + e.op + " INTERVAL ROUND(" + wrapTerm(r, e.r) + " * 86400) SECOND)", tTime
	case lt.numeric() && rt.numeric(), lt.class == cNull, rt.class == cNull:
		return g.plainSum(e, l, lt, r, rt)
	case lt.class == cStr || rt.class == cStr:
		g.note(e.line, "%s does arithmetic on text, which Oracle converts to a number on its own; check that %s does the same here", describeExpr(e), g.to)
	default:
		g.note(e.line, "%s: the types are not known here; if either side is a date, the calculation must be rewritten by hand, because Oracle counts in days", describeExpr(e))
	}
	s, _ := g.plainSum(e, l, lt, r, rt)
	return s, tUnknown
}

// pgUnits are MySQL's interval units as PostgreSQL spells an interval
// of one of them.
var pgUnits = map[string]string{
	"MICROSECOND": "1 microsecond", "SECOND": "1 second", "MINUTE": "1 minute", "HOUR": "1 hour",
	"DAY": "1 day", "WEEK": "1 week", "MONTH": "1 month", "QUARTER": "3 months", "YEAR": "1 year",
}

// wholeDays are the units that leave a DATE a DATE.
var wholeDays = map[string]bool{"DAY": true, "WEEK": true, "MONTH": true, "QUARTER": true, "YEAR": true}

func (g *gen) addMySQL(e *binExpr) (string, typ) {
	if iv, ok := e.r.(*intervalExpr); ok {
		return g.dateAdd(e.line, e.l, iv.n, iv.unit, e.op)
	}
	if iv, ok := e.l.(*intervalExpr); ok && e.op == "+" {
		return g.dateAdd(e.line, e.r, iv.n, iv.unit, "+")
	}
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	switch {
	case lt.temporal() || rt.temporal():
		g.refuse(e.line, "%s does arithmetic on a date as if it were a number", describeExpr(e))
	case lt.class == cStr || rt.class == cStr:
		g.note(e.line, "%s does arithmetic on text; MySQL reads the digits it starts with, PostgreSQL raises an error unless all of it is a number", describeExpr(e))
	}
	return g.plainSum(e, l, lt, r, rt)
}

// dateAdd writes MySQL's "date + INTERVAL n unit" for PostgreSQL.
func (g *gen) dateAdd(line int, date, n expr, unit, op string) (string, typ) {
	d, dt := g.value(date)
	ns, _ := g.value(n)
	one, ok := pgUnits[unit]
	if !ok {
		g.refuse(line, "the interval unit %s has no translation", unit)
		return "NULL", tUnknown
	}
	s := wrap(d, date) + " " + op + " " + wrapTerm(ns, n) + " * INTERVAL '" + one + "'"
	switch {
	case dt.class == cDate && wholeDays[unit]:
		// MySQL keeps a DATE a DATE; PostgreSQL makes it a timestamp.
		return "(" + s + ")::date", tDate
	case dt.class == cDate:
		return "(" + s + ")", typ{cTime, 0}
	case dt.temporal():
		return "(" + s + ")", dt
	}
	g.note(line, "an interval is added to %s, whose type is not known here; MySQL gives a date or a date and time depending on the value, PostgreSQL always a timestamp", describeExpr(date))
	return "(" + s + ")", tTime
}

var simpleInterval = regexp.MustCompile(`(?i)^\s*(-?[0-9]+)\s*(microsecond|second|sec|minute|min|hour|day|week|month|mon|year)s?\s*$`)

var intervalUnits = map[string]string{
	"microsecond": "MICROSECOND", "second": "SECOND", "sec": "SECOND", "minute": "MINUTE", "min": "MINUTE",
	"hour": "HOUR", "day": "DAY", "week": "WEEK", "month": "MONTH", "mon": "MONTH", "year": "YEAR",
}

// intervalOf reads an expression as a count of one unit, the only kind
// of interval MySQL can add to a date: INTERVAL '3 days', '1 hour'::interval,
// n * INTERVAL '1 day', INTERVAL '5' DAY.
func (g *gen) intervalOf(e expr) (n, unit string, ok bool) {
	switch e := e.(type) {
	case *parenExpr:
		if len(e.items) == 1 {
			return g.intervalOf(e.items[0])
		}
	case *litExpr:
		if e.kind != "interval" {
			return "", "", false
		}
		if e.unit != "" {
			if _, plain := pgUnits[e.unit]; plain && regexp.MustCompile(`^\s*-?[0-9]+\s*$`).MatchString(e.text) {
				return strings.TrimSpace(e.text), e.unit, true
			}
			return "", "", false
		}
		if m := simpleInterval.FindStringSubmatch(e.text); m != nil {
			return m[1], intervalUnits[strings.ToLower(m[2])], true
		}
	case *castExpr:
		if text, isStr := isLit(e.x, "string"); isStr && e.typ.name == "INTERVAL" {
			return g.intervalOf(&litExpr{kind: "interval", text: text, line: e.line})
		}
	case *binExpr:
		if e.op != "*" {
			return "", "", false
		}
		count, iv := e.l, e.r
		if _, _, isIv := g.intervalOf(e.l); isIv {
			count, iv = e.r, e.l
		}
		one, u, isIv := g.intervalOf(iv)
		if !isIv {
			return "", "", false
		}
		s, t := g.value(count)
		if !t.whole() {
			return "", "", false
		}
		if one != "1" {
			s = wrapTerm(s, count) + " * " + one
		} else {
			s = wrapTerm(s, count)
		}
		return s, u, true
	}
	return "", "", false
}

func (g *gen) addPostgres(e *binExpr) (string, typ) {
	if n, unit, ok := g.intervalOf(e.r); ok {
		l, lt := g.value(e.l)
		s := l + " " + e.op + " INTERVAL " + n + " " + unit
		switch {
		case lt.class == cDate:
			// PostgreSQL makes a date plus an interval a timestamp.
			return "CAST(" + s + " AS DATETIME)", tTime
		case lt.temporal():
			return "(" + s + ")", lt
		}
		g.note(e.line, "an interval is added to %s, whose type is not known here; PostgreSQL gives a timestamp, MySQL a date when the value is a date", describeExpr(e.l))
		return "(" + s + ")", tTime
	}
	l, lt := g.value(e.l)
	r, rt := g.value(e.r)
	switch {
	case lt.class == cInterval || rt.class == cInterval:
		g.refuse(e.line, "the interval in %s is not a plain count of one unit, which is all MySQL can add to a date", describeExpr(e))
		return "NULL", tUnknown
	case lt.class == cDate && rt.class == cDate && e.op == "-":
		return "DATEDIFF(" + l + ", " + r + ")", tInt
	case lt.class == cDate && rt.whole():
		return "(" + l + " " + e.op + " INTERVAL " + wrapTerm(r, e.r) + " DAY)", tDate
	case lt.temporal() || rt.temporal():
		g.refuse(e.line, "%s gives an interval, a type MySQL does not have", describeExpr(e))
		return "NULL", tUnknown
	case lt.numeric() && rt.numeric(), lt.class == cNull, rt.class == cNull:
	case (lt.class == cNum || lt.class == cFloat) || (rt.class == cNum || rt.class == cFloat):
		// A date can only be added to an integer, so this is arithmetic.
	default:
		g.note(e.line, "%s: the types are not known here; if either side is a date, the calculation must be rewritten by hand, because MySQL would add the numbers", describeExpr(e))
	}
	return g.plainSum(e, l, lt, r, rt)
}

// Date formats.

// oracleDateFormat rewrites an Oracle date format for PostgreSQL's
// to_char and to_timestamp, and returns the first element that is not
// among those the two read the same way. The one element that has to
// change is FM: in Oracle it switches padding off for everything after
// it, and on again when it occurs a second time; in PostgreSQL it only
// affects the element it stands in front of.
func oracleDateFormat(f string) (out, bad string) {
	up := strings.ToUpper(f)
	var b strings.Builder
	plain := false
	for i := 0; i < len(up); {
		if up[i] == '"' {
			end := strings.IndexByte(up[i+1:], '"')
			if end < 0 {
				return f, "an unclosed quote"
			}
			b.WriteString(f[i : i+end+2])
			i += end + 2
			continue
		}
		if !(up[i] >= 'A' && up[i] <= 'Z') {
			b.WriteByte(f[i])
			i++
			continue
		}
		if strings.HasPrefix(up[i:], "FM") {
			plain = !plain
			i += 2
			continue
		}
		matched := false
		for _, el := range []string{"YYYY", "MONTH", "HH24", "HH12", "DDD", "MON", "DAY", "FF1", "FF2", "FF3", "FF4", "FF5", "FF6", "YY", "MM", "DD", "DY", "HH", "MI", "SS", "IW", "AM", "PM", "Q"} {
			if strings.HasPrefix(up[i:], el) {
				if plain && el != "AM" && el != "PM" {
					b.WriteString("FM")
				}
				// The case of the element decides the case of names.
				b.WriteString(f[i : i+len(el)])
				i += len(el)
				matched = true
				break
			}
		}
		if !matched {
			end := i
			for end < len(up) && up[end] >= 'A' && up[end] <= 'Z' {
				end++
			}
			if bad == "" {
				bad = up[i:end]
			}
			b.WriteString(f[i:end])
			i = end
		}
	}
	return b.String(), bad
}

// numberFormat reports whether a format is a number format made only of
// the elements Oracle and PostgreSQL read the same way.
func numberFormat(f string) bool {
	if f == "" {
		return false
	}
	rest := strings.TrimPrefix(strings.ToUpper(f), "FM")
	for _, c := range rest {
		if !strings.ContainsRune("09.,", c) {
			return false
		}
	}
	return strings.ContainsAny(rest, "09")
}

// oracleToMySQLFormat rewrites an Oracle date format for DATE_FORMAT. It
// only knows the numeric elements; the names of months and days come
// out in different case in the two databases.
func oracleToMySQLFormat(f string) (string, string) {
	up := strings.ToUpper(f)
	var b strings.Builder
	elements := []struct{ ora, my string }{
		{"YYYY", "%Y"}, {"HH24", "%H"}, {"HH12", "%h"}, {"YY", "%y"}, {"MM", "%m"}, {"DD", "%d"}, {"HH", "%h"}, {"MI", "%i"}, {"SS", "%s"},
	}
	for i := 0; i < len(up); {
		c := up[i]
		if c == '%' {
			b.WriteString("%%")
			i++
			continue
		}
		if !(c >= 'A' && c <= 'Z') {
			if c == '"' {
				return "", "quoted text"
			}
			b.WriteByte(f[i])
			i++
			continue
		}
		matched := false
		for _, el := range elements {
			if strings.HasPrefix(up[i:], el.ora) {
				b.WriteString(el.my)
				i += len(el.ora)
				matched = true
				break
			}
		}
		if !matched {
			end := i
			for end < len(up) && up[end] >= 'A' && up[end] <= 'Z' {
				end++
			}
			return "", up[i:end]
		}
	}
	return b.String(), ""
}

// mysqlToPGFormat rewrites a DATE_FORMAT format for to_char.
func mysqlToPGFormat(f string) (string, string) {
	spec := map[byte]string{
		'Y': "YYYY", 'y': "YY", 'm': "MM", 'c': "FMMM", 'd': "DD", 'e': "FMDD", 'H': "HH24", 'h': "HH12", 'I': "HH12",
		'i': "MI", 's': "SS", 'S': "SS", 'f': "US", 'M': "FMMonth", 'b': "Mon", 'W': "FMDay", 'a': "Dy", 'j': "DDD",
		'p': "AM", 'T': "HH24:MI:SS", '%': "%",
	}
	var b strings.Builder
	var text strings.Builder
	flush := func() {
		if text.Len() == 0 {
			return
		}
		// Letters outside the elements must be quoted, or to_char would
		// read them as elements.
		if strings.ContainsAny(text.String(), "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			b.WriteString(`"` + strings.ReplaceAll(text.String(), `"`, `\"`) + `"`)
		} else {
			b.WriteString(text.String())
		}
		text.Reset()
	}
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			text.WriteByte(f[i])
			continue
		}
		i++
		if i >= len(f) {
			return "", "a lone %"
		}
		out, ok := spec[f[i]]
		if !ok {
			return "", "%" + string(f[i])
		}
		if f[i] == '%' {
			text.WriteByte('%')
			continue
		}
		flush()
		b.WriteString(out)
	}
	flush()
	return b.String(), ""
}

// pgToMySQLFormat rewrites a to_char date format for DATE_FORMAT.
func pgToMySQLFormat(f string) (string, string) {
	elements := []struct{ pg, my string }{
		{"YYYY", "%Y"}, {"HH24", "%H"}, {"HH12", "%h"}, {"FMMM", "%c"}, {"FMDD", "%e"}, {"YY", "%y"}, {"MM", "%m"}, {"DD", "%d"},
		{"HH", "%h"}, {"MI", "%i"}, {"SS", "%s"}, {"US", "%f"},
	}
	up := strings.ToUpper(f)
	var b strings.Builder
	for i := 0; i < len(up); {
		c := up[i]
		if c == '%' {
			b.WriteString("%%")
			i++
			continue
		}
		if !(c >= 'A' && c <= 'Z') {
			if c == '"' {
				return "", "quoted text"
			}
			b.WriteByte(f[i])
			i++
			continue
		}
		matched := false
		for _, el := range elements {
			if strings.HasPrefix(up[i:], el.pg) {
				b.WriteString(el.my)
				i += len(el.pg)
				matched = true
				break
			}
		}
		if !matched {
			end := i
			for end < len(up) && up[end] >= 'A' && up[end] <= 'Z' {
				end++
			}
			return "", up[i:end]
		}
	}
	return b.String(), ""
}
