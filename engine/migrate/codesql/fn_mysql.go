package codesql

import (
	"strings"
)

// mysqlPG are the built-in functions of MySQL written for PostgreSQL.
var mysqlPG map[string]fnRule

func init() {
	mysqlPG = map[string]fnRule{
		"IFNULL":            sameArg("COALESCE", 2),
		"COALESCE":          sameArg("COALESCE"),
		"NULLIF":            sameArg("NULLIF", 2),
		"GREATEST":          nullsDiffer("GREATEST"),
		"LEAST":             nullsDiffer("LEAST"),
		"IF":                myIf,
		"ISNULL":            myIsNull,
		"CONCAT":            myConcat,
		"CONCAT_WS":         myConcatWS,
		"UPPER":             myText("upper"),
		"UCASE":             myText("upper"),
		"LOWER":             myText("lower"),
		"LCASE":             myText("lower"),
		"REVERSE":           myText("reverse"),
		"LENGTH":            same("octet_length", tInt, 1),
		"CHAR_LENGTH":       same("char_length", tInt, 1),
		"CHARACTER_LENGTH":  same("char_length", tInt, 1),
		"SUBSTRING":         mySubstring,
		"SUBSTR":            mySubstring,
		"MID":               mySubstring,
		"LEFT":              myLeft("left"),
		"RIGHT":             myLeft("right"),
		"TRIM":              myTrim("btrim"),
		"LTRIM":             myTrim("ltrim"),
		"RTRIM":             myTrim("rtrim"),
		"REPLACE":           myTextN("replace", 3),
		"LPAD":              myTextN("lpad", 3),
		"RPAD":              myTextN("rpad", 3),
		"REPEAT":            myTextN("repeat", 2),
		"INSTR":             myInstr(false),
		"LOCATE":            myInstr(true),
		"POSITION":          myInstr(true),
		"ROUND":             myRound,
		"TRUNCATE":          myTruncate,
		"FLOOR":             myWhole("floor"),
		"CEIL":              myWhole("ceil"),
		"CEILING":           myWhole("ceil"),
		"ABS":               sameArg("abs", 1),
		"SIGN":              same("sign", tInt, 1),
		"SQRT":              same("sqrt", tFloat, 1),
		"POW":               same("power", tFloat, 2),
		"POWER":             same("power", tFloat, 2),
		"MOD":               myMod,
		"COUNT":             aggregate("COUNT"),
		"SUM":               aggregate("SUM"),
		"MIN":               aggregate("MIN"),
		"MAX":               aggregate("MAX"),
		"AVG":               average,
		"NOW":               myClock(pgStatementSeconds, typ{cTime, 0}),
		"CURRENT_TIMESTAMP": myClock(pgStatementSeconds, typ{cTime, 0}),
		"LOCALTIME":         myClock(pgStatementSeconds, typ{cTime, 0}),
		"LOCALTIMESTAMP":    myClock(pgStatementSeconds, typ{cTime, 0}),
		"SYSDATE":           myClock(pgClockSeconds, typ{cTime, 0}),
		"CURDATE":           myClock("statement_timestamp()::date", tDate),
		"CURRENT_DATE":      myClock("statement_timestamp()::date", tDate),
		"UTC_TIMESTAMP":     myClock("date_trunc('second', statement_timestamp() AT TIME ZONE 'UTC')::timestamp(0)", typ{cTime, 0}),
		"DATE_ADD":          myDateAdd("+"),
		"ADDDATE":           myDateAdd("+"),
		"DATE_SUB":          myDateAdd("-"),
		"SUBDATE":           myDateAdd("-"),
		"DATEDIFF":          myDateDiff,
		"TIMESTAMPDIFF":     myTimestampDiff,
		"DATE_FORMAT":       myDateFormat,
		"DATE":              myDate,
		"LAST_DAY":          myLastDay,
		"YEAR":              myField("YEAR"),
		"MONTH":             myField("MONTH"),
		"DAY":               myField("DAY"),
		"DAYOFMONTH":        myField("DAY"),
		"HOUR":              myField("HOUR"),
		"MINUTE":            myField("MINUTE"),
		"SECOND":            myField("SECOND"),
		"QUARTER":           myField("QUARTER"),
		"DAYOFYEAR":         myField("DOY"),
		"DAYOFWEEK":         myField("DOW"),
		"WEEKDAY":           myField("ISODOW"),
		"LAST_INSERT_ID":    myLastInsertID,
	}
}

// asMoment writes a MySQL value where PostgreSQL needs a date or a time.
// MySQL takes text there and reads it as a date; PostgreSQL wants the
// conversion spelled out.
func (g *gen) asMoment(a argv) string {
	if a.t.temporal() {
		return a.s
	}
	return wrap(a.s, a.e) + "::timestamp"
}

func myText(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		return name + "(" + g.asText(a[0].e, a[0].s, a[0].t, "given to "+strings.ToUpper(name)) + ")", tStr, true
	}
}

// myTextN keeps a function whose first argument is text and that takes
// n arguments in all.
func myTextN(name string, n int) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != n {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "given to "+strings.ToUpper(name))
		return name + "(" + joinArgs(a) + ")", tStr, true
	}
}

// myIf writes IF(condition, a, b).
func myIf(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 3 {
		return "", tUnknown, false
	}
	cond := g.cond(a[0].e)
	yes, t := g.value(a[1].e)
	no, t2 := g.value(a[2].e)
	if !t.known() || t.class == cNull {
		t = t2
	}
	return "CASE WHEN " + cond + " THEN " + yes + " ELSE " + no + " END", t, true
}

func myIsNull(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	return "(" + wrap(a[0].s, a[0].e) + " IS NULL)::int", tInt, true
}

// myConcat writes CONCAT, which is NULL as soon as one argument is, as
// "||" is in PostgreSQL.
func myConcat(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) == 0 {
		return "", tUnknown, false
	}
	var parts []string
	for _, arg := range a {
		s := g.asText(arg.e, arg.s, arg.t, "joined to text")
		if s == arg.s && !isAtom(arg.e) {
			s = "(" + s + ")"
		}
		if arg.t.class == cNull {
			s = "NULL::text"
		}
		parts = append(parts, s)
	}
	if len(parts) == 1 {
		return parts[0], tStr, true
	}
	return "(" + strings.Join(parts, " || ") + ")", tStr, true
}

func myConcatWS(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) < 2 {
		return "", tUnknown, false
	}
	parts := []string{a[0].s}
	for _, arg := range a[1:] {
		parts = append(parts, g.asText(arg.e, arg.s, arg.t, "joined to text"))
	}
	return "concat_ws(" + strings.Join(parts, ", ") + ")", tStr, true
}

// mySubstring writes SUBSTRING. MySQL counts a negative start from the
// end and gives nothing for a start of 0; PostgreSQL does neither.
func mySubstring(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 && len(a) != 3 {
		return "", tUnknown, false
	}
	a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "cut with SUBSTRING")
	if !positive(a[1]) {
		g.note(c.line, "SUBSTRING starts at %s; if that can be 0 or negative, MySQL gives nothing or counts from the end, and PostgreSQL does neither", describeExpr(a[1].e))
	}
	if len(a) == 3 {
		if n, ok := intLit(a[2].e); !ok || n < 0 {
			g.note(c.line, "SUBSTRING takes %s characters; if that can be negative, MySQL gives nothing and PostgreSQL raises an error", describeExpr(a[2].e))
		}
	}
	return "substr(" + joinArgs(a) + ")", tStr, true
}

// myLeft writes LEFT and RIGHT. A negative count means "none" in MySQL
// and "all but that many" in PostgreSQL.
func myLeft(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "cut with "+strings.ToUpper(name))
		if n, ok := intLit(a[1].e); !ok || n < 0 {
			g.note(c.line, "%s takes %s characters; if that can be negative, MySQL gives nothing and PostgreSQL gives all but that many", strings.ToUpper(name), describeExpr(a[1].e))
		}
		return name + "(" + joinArgs(a) + ")", tStr, true
	}
}

func myTrim(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		switch len(a) {
		case 1:
			return name + "(" + a[0].s + ", ' ')", tStr, true
		case 2:
			// MySQL removes a string, PostgreSQL any of a set of
			// characters; the two agree for a single character.
			if text, ok := literalText(a[1]); ok && len([]rune(text)) == 1 {
				return name + "(" + a[0].s + ", " + a[1].s + ")", tStr, true
			}
		}
		return "", tUnknown, false
	}
}

// myInstr writes INSTR(text, part), and LOCATE and POSITION, which take
// the two the other way round.
func myInstr(partFirst bool) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 {
			return "", tUnknown, false
		}
		g.textCompare(c.line, tStr, tStr, strings.ToUpper(c.name[0].name))
		if partFirst {
			a[0], a[1] = a[1], a[0]
		}
		return "strpos(" + a[0].s + ", " + a[1].s + ")", tInt, true
	}
}

func myRound(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	x := a[0].s
	switch {
	case a[0].t.class == cFloat:
		g.note(c.line, "ROUND of a floating point number: MySQL and PostgreSQL may round a half differently")
		x = wrap(x, a[0].e) + "::numeric"
	case a[0].t.class == cInt && len(a) == 2:
		x = wrap(x, a[0].e) + "::numeric"
	case !a[0].t.known():
		g.note(c.line, "ROUND is applied to %s, whose type is not known here; if it is a floating point number PostgreSQL needs a cast and may round a half differently", describeExpr(a[0].e))
	}
	if len(a) == 1 {
		if a[0].t.class == cInt {
			return x, tInt, true
		}
		return "round(" + x + ")", tWhole, true
	}
	t := tNum
	if n, ok := intLit(a[1].e); ok && n >= 0 {
		t = typ{cNum, n}
	}
	return "round(" + x + ", " + a[1].s + ")", t, true
}

func myTruncate(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	x := a[0].s
	if a[0].t.class != cNum {
		x = wrap(x, a[0].e) + "::numeric"
	}
	t := tNum
	if n, ok := intLit(a[1].e); ok && n >= 0 {
		t = typ{cNum, n}
	}
	return "trunc(" + x + ", " + a[1].s + ")", t, true
}

func myWhole(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		if a[0].t.class == cInt {
			return a[0].s, tInt, true
		}
		return name + "(" + a[0].s + ")", tWhole, true
	}
}

func myMod(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	s, t := g.division(&binExpr{op: "%", l: a[0].e, r: a[1].e, line: c.line})
	return "(" + s + ")", t, true
}

func myClock(text string, t typ) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 0 {
			return "", tUnknown, false
		}
		return text, t, true
	}
}

// myDateAdd writes DATE_ADD(d, INTERVAL n unit) and its relatives.
func myDateAdd(op string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 {
			return "", tUnknown, false
		}
		if iv, ok := a[1].e.(*intervalExpr); ok {
			s, t := g.dateAdd(c.line, a[0].e, iv.n, iv.unit, op)
			return s, t, true
		}
		// ADDDATE(d, n) adds days.
		if name := strings.ToUpper(c.name[0].name); name != "ADDDATE" && name != "SUBDATE" {
			return "", tUnknown, false
		}
		s, t := g.dateAdd(c.line, a[0].e, a[1].e, "DAY", op)
		return s, t, true
	}
}

func myDateDiff(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	day := func(x argv) string {
		if x.t.class == cDate {
			return x.s
		}
		return wrap(x.s, x.e) + "::date"
	}
	return "(" + day(a[0]) + " - " + day(a[1]) + ")", tInt, true
}

// myTimestampDiff writes TIMESTAMPDIFF(unit, from, to), the number of
// whole units between two moments.
func myTimestampDiff(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 3 {
		return "", tUnknown, false
	}
	unit, ok := a[0].e.(*nameExpr)
	if !ok {
		return "", tUnknown, false
	}
	var from, to argv
	from.e, to.e = a[1].e, a[2].e
	from.s, from.t = g.value(a[1].e)
	to.s, to.t = g.value(a[2].e)
	if from.t.class == cTimeTZ || to.t.class == cTimeTZ {
		g.note(c.line, "TIMESTAMPDIFF of a TIMESTAMP column: MySQL counts in the session's time zone, where a day can have 23 or 25 hours")
	}
	seconds := "EXTRACT(EPOCH FROM (" + g.asMoment(to) + " - " + g.asMoment(from) + "))"
	months := "(EXTRACT(YEAR FROM age(" + g.asMoment(to) + ", " + g.asMoment(from) + ")) * 12 + EXTRACT(MONTH FROM age(" + g.asMoment(to) + ", " + g.asMoment(from) + ")))"
	switch strings.ToUpper(unit.parts[0].name) {
	case "SECOND":
		return "trunc(" + seconds + ")::bigint", tInt, true
	case "MINUTE":
		return "trunc(" + seconds + " / 60)::bigint", tInt, true
	case "HOUR":
		return "trunc(" + seconds + " / 3600)::bigint", tInt, true
	case "DAY":
		return "trunc(" + seconds + " / 86400)::bigint", tInt, true
	case "WEEK":
		return "trunc(" + seconds + " / 604800)::bigint", tInt, true
	case "MONTH":
		g.note(c.line, "TIMESTAMPDIFF(MONTH ...) is written with age(); the two count the days at the end of a month differently in some cases")
		return months + "::int", tInt, true
	case "YEAR":
		g.note(c.line, "TIMESTAMPDIFF(YEAR ...) is written with age(); the two count the days at the end of a month differently in some cases")
		return "trunc(" + months + " / 12)::int", tInt, true
	}
	return "", tUnknown, false
}

func myDateFormat(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	format, ok := literalText(a[1])
	if !ok {
		g.refuse(c.line, "DATE_FORMAT with a format that is not a literal")
		return "NULL", tStr, true
	}
	pg, bad := mysqlToPGFormat(format)
	if bad != "" {
		g.refuse(c.line, "the date format element %s has no PostgreSQL counterpart", bad)
		return "NULL", tStr, true
	}
	return "to_char(" + g.asMoment(a[0]) + ", " + g.stringLit(pg) + ")", tStr, true
}

func myDate(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	return wrap(a[0].s, a[0].e) + "::date", tDate, true
}

func myLastDay(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	return "(date_trunc('month', " + g.asMoment(a[0]) + ") + INTERVAL '1 month - 1 day')::date", tDate, true
}

// myField writes YEAR(d), MONTH(d) and the like.
func myField(field string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		x := "EXTRACT(" + field + " FROM " + g.asMoment(a[0]) + ")"
		switch field {
		case "DOW":
			// MySQL numbers the days from Sunday = 1.
			return "(" + x + "::int + 1)", tInt, true
		case "ISODOW":
			// WEEKDAY numbers them from Monday = 0.
			return "(" + x + "::int - 1)", tInt, true
		case "SECOND":
			return "trunc(" + x + ")::int", tInt, true
		}
		return x + "::int", tInt, true
	}
}

func myLastInsertID(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 0 {
		return "", tUnknown, false
	}
	g.note(c.line, "LAST_INSERT_ID() becomes lastval(), which is the last value any sequence gave this session; it fails if no sequence was used yet, where MySQL gives 0")
	return "lastval()", tInt, true
}
