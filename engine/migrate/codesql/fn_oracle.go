package codesql

import (
	"strings"
)

// The built-in functions of Oracle, for both targets. Most rules are the
// same idea with two spellings; pick chooses the spelling.

func (g *gen) pick(pg, my string) string {
	if g.to == Postgres {
		return pg
	}
	return my
}

// oraclePG and oracleMy are the rules for each target. They are filled in
// when the package starts, because the rules call back into the code
// that looks them up.
var oraclePG, oracleMy map[string]fnRule

func init() {
	// The rules both targets share.
	common := map[string]fnRule{
		"COALESCE": sameArg("COALESCE"),
		"NULLIF":   sameArg("NULLIF", 2),
		"GREATEST": oraGreatest("GREATEST"),
		"LEAST":    oraGreatest("LEAST"),
		"UPPER":    oraText("UPPER", 1),
		"LOWER":    oraText("LOWER", 1),
		"ABS":      sameArg("ABS", 1),
		"SIGN":     same("SIGN", tInt, 1),
		"SQRT":     same("SQRT", tNum, 1),
		"POWER":    same("POWER", tNum, 2),
		"CEIL":     oraWhole("CEIL"),
		"FLOOR":    oraWhole("FLOOR"),
		"COUNT":    aggregate("COUNT"),
		"SUM":      aggregate("SUM"),
		"MIN":      aggregate("MIN"),
		"MAX":      aggregate("MAX"),
		"AVG":      average,
		"NVL":      oraNVL,
		"NVL2":     oraNVL2,
		"DECODE":   oraDecode,
		"LENGTH":   oraLength,
		"SUBSTR":   oraSubstr,
		"INSTR":    oraInstr,
		"LTRIM":    oraTrim("LTRIM"),
		"RTRIM":    oraTrim("RTRIM"),
		"TRIM":     oraTrim("TRIM"),
		"LPAD":     oraPad("LPAD"),
		"RPAD":     oraPad("RPAD"),
		"REPLACE":  oraReplace,
		"CONCAT":   oraConcat,
		"TO_CHAR":  oraToChar,
		"TO_NUMBER": func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
			if len(a) != 1 {
				return "", tUnknown, false
			}
			if a[0].t.numeric() {
				return a[0].s, a[0].t, true
			}
			if g.to == Postgres {
				return "CAST(" + a[0].s + " AS numeric)", tNum, true
			}
			g.note(c.line, "TO_NUMBER raises an error in Oracle when the text is not a number; MySQL reads the digits it starts with and gives 0 for the rest")
			return "CAST(" + a[0].s + " AS DECIMAL(65,30))", typ{cNum, 30}, true
		},
		"TO_DATE":    oraToDate,
		"TRUNC":      oraTrunc,
		"ROUND":      oraRound,
		"MOD":        oraMod,
		"ADD_MONTHS": oraAddMonths,
		"LAST_DAY":   oraLastDay,
		"UPDATING":   oraUpdating,
	}

	oraclePG = merge(common, map[string]fnRule{
		"INITCAP": oraText("INITCAP", 1),
		"CHR":     same("chr", tStr, 1),
		"ASCII":   same("ascii", tInt, 1),
		"LISTAGG": oraListagg,
	})
	oracleMy = merge(common, map[string]fnRule{
		"ASCII": same("ASCII", tInt, 1),
	})
}

func merge(maps ...map[string]fnRule) map[string]fnRule {
	out := map[string]fnRule{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// oraText keeps a function that takes text and gives text. A result
// that can be empty is turned into NULL, as Oracle would have it.
func oraText(name string, n int) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != n {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "given to "+name)
		return g.pick(strings.ToLower(name), name) + "(" + joinArgs(a) + ")", tStr, true
	}
}

func oraGreatest(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if g.to == MySQL {
			// MySQL gives NULL for a NULL argument, as Oracle does.
			return sameArg(name)(g, c, a)
		}
		return nullsDiffer(name)(g, c, a)
	}
}

func oraWhole(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		return name + "(" + a[0].s + ")", tWhole, true
	}
}

func oraNVL(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	return sameArg("COALESCE")(g, c, a)
}

func oraNVL2(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 3 {
		return "", tUnknown, false
	}
	t := a[1].t
	if !t.known() || t.class == cNull {
		t = a[2].t
	}
	return "CASE WHEN " + wrap(a[0].s, a[0].e) + " IS NOT NULL THEN " + a[1].s + " ELSE " + a[2].s + " END", t, true
}

// oraDecode writes DECODE as a CASE. DECODE counts two NULLs as equal,
// which "=" does not, so a value that is not a plain literal is compared
// with the operator that does.
func oraDecode(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) < 3 {
		return "", tUnknown, false
	}
	plain := true
	for i := 1; i+1 < len(a); i += 2 {
		if l, ok := a[i].e.(*litExpr); !ok || l.kind == "null" || (l.kind == "string" && l.text == "") {
			plain = false
		}
	}
	var b strings.Builder
	result := tUnknown
	if plain {
		b.WriteString("CASE " + a[0].s)
	} else {
		b.WriteString("CASE")
	}
	for i := 1; i+1 < len(a); i += 2 {
		switch {
		case plain:
			b.WriteString(" WHEN " + a[i].s)
		case g.to == Postgres:
			b.WriteString(" WHEN " + wrap(a[0].s, a[0].e) + " IS NOT DISTINCT FROM " + wrap(a[i].s, a[i].e))
		default:
			b.WriteString(" WHEN " + wrap(a[0].s, a[0].e) + " <=> " + wrap(a[i].s, a[i].e))
		}
		b.WriteString(" THEN " + a[i+1].s)
		if !result.known() || result.class == cNull {
			result = a[i+1].t
		}
	}
	if len(a)%2 == 0 {
		b.WriteString(" ELSE " + a[len(a)-1].s)
	}
	b.WriteString(" END")
	return b.String(), result, true
}

func oraLength(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	s := g.asText(a[0].e, a[0].s, a[0].t, "measured with LENGTH")
	return g.pick("length(", "CHAR_LENGTH(") + s + ")", tInt, true
}

// oraSubstr writes SUBSTR. Oracle counts a start of 0 as 1 and a
// negative start from the end; PostgreSQL does neither, and MySQL only
// the second.
func oraSubstr(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 && len(a) != 3 {
		return "", tUnknown, false
	}
	a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "cut with SUBSTR")
	start, lit := intLit(a[1].e)
	switch {
	case !lit && atLeastOne(a[1].e):
	case lit && start == 0:
		a[1].s = "1"
	case lit && start > 0:
	case lit && g.to == MySQL:
	case lit:
		g.note(c.line, "SUBSTR starts %d characters from the end; when the text is shorter than that Oracle gives NULL and the converted code gives all of it", -start)
		if len(a) == 2 {
			return emptyIsNull("right(" + a[0].s + ", " + a[1].s[1:] + ")"), tStr, true
		}
		return emptyIsNull("substr(right(" + a[0].s + ", " + a[1].s[1:] + "), 1, " + a[2].s + ")"), tStr, true
	case g.to == MySQL:
		g.note(c.line, "SUBSTR starts at %s; if that can be 0, Oracle starts at the first character and MySQL gives nothing", describeExpr(a[1].e))
	default:
		g.note(c.line, "SUBSTR starts at %s; if that can be 0 or negative, Oracle starts at the first character or counts from the end, and PostgreSQL does neither", describeExpr(a[1].e))
	}
	return emptyIsNull(g.pick("substr(", "SUBSTRING(") + joinArgs(a) + ")"), tStr, true
}

// atLeastOne reports whether an expression is plainly 1 or more: a
// position found by INSTR, or a length, plus a positive literal.
func atLeastOne(e expr) bool {
	b, ok := e.(*binExpr)
	if !ok || b.op != "+" {
		return false
	}
	count, n := b.l, b.r
	if _, isLit := intLit(count); isLit {
		count, n = n, count
	}
	if v, isLit := intLit(n); !isLit || v < 1 {
		return false
	}
	c, ok := count.(*callExpr)
	if !ok || len(c.name) != 1 {
		return false
	}
	name := strings.ToUpper(c.name[0].name)
	return name == "INSTR" || name == "LENGTH"
}

func oraInstr(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) == 3 {
		if n, ok := intLit(a[2].e); ok && n == 1 {
			a = a[:2]
		}
	}
	if len(a) != 2 {
		return "", tUnknown, false
	}
	if g.to == Postgres {
		return "strpos(" + a[0].s + ", " + a[1].s + ")", tInt, true
	}
	return "INSTR(" + a[0].s + ", " + a[1].s + ")", tInt, true
}

func oraTrim(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 && len(a) != 2 {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "trimmed")
		if len(a) == 1 {
			return emptyIsNull(g.pick(strings.ToLower(name), name) + "(" + a[0].s + ")"), tStr, true
		}
		if g.to == Postgres {
			fn := map[string]string{"TRIM": "btrim", "LTRIM": "ltrim", "RTRIM": "rtrim"}[name]
			return emptyIsNull(fn + "(" + a[0].s + ", " + a[1].s + ")"), tStr, true
		}
		// MySQL removes a string, not any of a set of characters; the
		// two are the same for a single character.
		if text, ok := literalText(a[1]); !ok || len([]rune(text)) != 1 {
			return "", tUnknown, false
		}
		side := map[string]string{"TRIM": "BOTH", "LTRIM": "LEADING", "RTRIM": "TRAILING"}[name]
		return emptyIsNull("TRIM(" + side + " " + a[1].s + " FROM " + a[0].s + ")"), tStr, true
	}
}

func oraPad(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 && len(a) != 3 {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "padded")
		if len(a) == 2 && g.to == MySQL {
			a = append(a, argv{s: "' '"})
		}
		return emptyIsNull(g.pick(strings.ToLower(name), name) + "(" + joinArgs(a) + ")"), tStr, true
	}
}

// oraReplace writes REPLACE. Oracle reads a NULL second or third
// argument as "nothing"; the targets give NULL for the whole call.
func oraReplace(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 && len(a) != 3 {
		return "", tUnknown, false
	}
	a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "searched with REPLACE")
	if len(a) == 2 {
		a = append(a, argv{s: "''", e: &litExpr{kind: "string", text: "x"}})
	}
	for i := 1; i < 3; i++ {
		if text, ok := literalText(a[i]); ok && text != "" {
			continue
		}
		if a[i].s == "NULL" || a[i].s == "''" {
			a[i].s = "''"
			continue
		}
		a[i].s = "COALESCE(" + a[i].s + ", '')"
	}
	return emptyIsNull(g.pick("replace(", "REPLACE(") + joinArgs(a) + ")"), tStr, true
}

func oraConcat(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	s, t := g.concat(&binExpr{op: "||", l: a[0].e, r: a[1].e, line: c.line})
	return s, t, true
}

// oraToChar writes TO_CHAR.
func oraToChar(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	switch len(a) {
	case 1:
		return g.asText(a[0].e, a[0].s, a[0].t, "converted with TO_CHAR"), tStr, true
	case 2:
	default:
		return "", tUnknown, false
	}
	format, ok := literalText(a[1])
	if !ok {
		g.refuse(c.line, "TO_CHAR with a format that is not a literal")
		return "NULL", tStr, true
	}
	isNumber := a[0].t.numeric() || (!a[0].t.known() && numberFormat(format))
	if isNumber {
		if g.to == MySQL {
			g.refuse(c.line, "TO_CHAR of a number with a format: MySQL has no formats for numbers")
			return "NULL", tStr, true
		}
		if !numberFormat(format) {
			g.note(c.line, "the number format %s uses elements PostgreSQL may read differently from Oracle", g.stringLit(format))
		}
		return "to_char(" + a[0].s + ", " + a[1].s + ")", tStr, true
	}
	if !a[0].t.temporal() {
		g.note(c.line, "TO_CHAR formats %s, whose type is not known here, with %s; it is taken to be a date", describeExpr(a[0].e), g.stringLit(format))
	}
	if g.to == MySQL {
		my, bad := oracleToMySQLFormat(format)
		if bad != "" {
			g.refuse(c.line, "the date format element %s has no MySQL counterpart that gives the same text", bad)
			return "NULL", tStr, true
		}
		return "DATE_FORMAT(" + a[0].s + ", " + g.stringLit(my) + ")", tStr, true
	}
	pg, bad := oracleDateFormat(format)
	if bad != "" {
		g.note(c.line, "the date format element %s is not one PostgreSQL is known to read as Oracle does", bad)
	}
	return "to_char(" + a[0].s + ", " + g.stringLit(pg) + ")", tStr, true
}

func oraToDate(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	switch len(a) {
	case 1:
		if a[0].t.temporal() {
			return a[0].s, a[0].t, true
		}
		g.refuse(c.line, "TO_DATE without a format reads the text in the session's date format (NLS_DATE_FORMAT)")
		return "NULL", tTime, true
	case 2:
	default:
		return "", tUnknown, false
	}
	format, ok := literalText(a[1])
	if !ok {
		g.refuse(c.line, "TO_DATE with a format that is not a literal")
		return "NULL", tTime, true
	}
	if g.to == MySQL {
		my, bad := oracleToMySQLFormat(format)
		if bad != "" {
			g.refuse(c.line, "the date format element %s has no MySQL counterpart", bad)
			return "NULL", tTime, true
		}
		g.note(c.line, "TO_DATE raises an error in Oracle when the text does not fit the format; MySQL's STR_TO_DATE gives NULL instead")
		if strings.Contains(my, "%H") || strings.Contains(my, "%h") {
			return "STR_TO_DATE(" + a[0].s + ", " + g.stringLit(my) + ")", tTime, true
		}
		return "CAST(STR_TO_DATE(" + a[0].s + ", " + g.stringLit(my) + ") AS DATETIME)", tTime, true
	}
	pg, bad := oracleDateFormat(format)
	if bad != "" {
		g.note(c.line, "the date format element %s is not one PostgreSQL is known to read as Oracle does", bad)
	}
	return "to_timestamp(" + a[0].s + ", " + g.stringLit(pg) + ")::timestamp(0)", tTime, true
}

// truncUnits are the units Oracle's TRUNC of a date takes, by what the
// targets call them.
var truncUnits = map[string]string{
	"DD": "day", "DDD": "day", "J": "day", "MM": "month", "MON": "month", "MONTH": "month", "RM": "month",
	"YYYY": "year", "YYY": "year", "YY": "year", "Y": "year", "YEAR": "year", "SYYYY": "year",
	"Q": "quarter", "HH": "hour", "HH12": "hour", "HH24": "hour", "MI": "minute", "IW": "week",
}

// oraTrunc writes TRUNC, which cuts a number or a date depending on what
// it is given.
func oraTrunc(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	unit, hasUnit := "", false
	if len(a) == 2 {
		unit, hasUnit = literalText(a[1])
	}
	isDate := a[0].t.temporal() || hasUnit
	switch {
	case isDate:
		u := "day"
		if hasUnit {
			var ok bool
			if u, ok = truncUnits[strings.ToUpper(unit)]; !ok {
				g.refuse(c.line, "TRUNC of a date to %s: the result depends on the session's territory or has no counterpart", g.stringLit(unit))
				return "NULL", tTime, true
			}
		} else if len(a) == 2 {
			return "", tUnknown, false
		}
		if g.to == Postgres {
			return "date_trunc('" + u + "', " + a[0].s + ")", tTime, true
		}
		switch u {
		case "day":
			return "CAST(DATE(" + a[0].s + ") AS DATETIME)", tTime, true
		case "month":
			return "CAST(DATE_FORMAT(" + a[0].s + ", '%Y-%m-01') AS DATETIME)", tTime, true
		case "year":
			return "CAST(DATE_FORMAT(" + a[0].s + ", '%Y-01-01') AS DATETIME)", tTime, true
		case "hour":
			return "CAST(DATE_FORMAT(" + a[0].s + ", '%Y-%m-%d %H:00:00') AS DATETIME)", tTime, true
		case "minute":
			return "CAST(DATE_FORMAT(" + a[0].s + ", '%Y-%m-%d %H:%i:00') AS DATETIME)", tTime, true
		}
		g.refuse(c.line, "TRUNC of a date to the %s has no MySQL form here", u)
		return "NULL", tTime, true
	case !a[0].t.known():
		g.note(c.line, "TRUNC is applied to %s, whose type is not known here; it is taken to be a number, and a date would need date_trunc", describeExpr(a[0].e))
	}
	t := tWhole
	if len(a) == 2 {
		t = tNum
		if n, ok := intLit(a[1].e); ok && n >= 0 {
			t = typ{cNum, n}
		}
	}
	if g.to == MySQL {
		if len(a) == 1 {
			return "TRUNCATE(" + a[0].s + ", 0)", t, true
		}
		return "TRUNCATE(" + joinArgs(a) + ")", t, true
	}
	if a[0].t.class == cFloat {
		a[0].s = wrap(a[0].s, a[0].e) + "::numeric"
	}
	return "trunc(" + joinArgs(a) + ")", t, true
}

func oraRound(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	switch {
	case a[0].t.temporal():
		g.refuse(c.line, "ROUND of a date")
		return "NULL", tTime, true
	case !a[0].t.known() && len(a) == 1:
		g.note(c.line, "ROUND is applied to %s, whose type is not known here; it is taken to be a number", describeExpr(a[0].e))
	case a[0].t.class == cFloat:
		g.note(c.line, "ROUND of a binary floating point number rounds halves away from zero in Oracle; check the result on %s", g.to)
	}
	t := tWhole
	if len(a) == 2 {
		t = tNum
		if n, ok := intLit(a[1].e); ok && n >= 0 {
			t = typ{cNum, n}
		}
	}
	if g.to == Postgres && a[0].t.class == cFloat {
		a[0].s = wrap(a[0].s, a[0].e) + "::numeric"
	}
	return g.pick("round(", "ROUND(") + joinArgs(a) + ")", t, true
}

// oraMod writes MOD. Oracle gives the dividend back when the divisor is
// 0; PostgreSQL raises an error and MySQL gives NULL.
func oraMod(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	if !nonZeroLit(a[1].e) {
		g.note(c.line, "MOD gives its first argument in Oracle when the second is 0; %s does not", g.to)
	}
	return g.pick("mod(", "MOD(") + joinArgs(a) + ")", numericResult(a[0].t, a[1].t, false), true
}

func oraAddMonths(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	g.note(c.line, "ADD_MONTHS moves the last day of a month to the last day of the resulting month (28 February plus one month is 31 March in Oracle); %s keeps the day number", g.to)
	if g.to == Postgres {
		return "(" + a[0].s + " + " + wrapTerm(a[1].s, a[1].e) + " * INTERVAL '1 month')", tTime, true
	}
	return "(" + a[0].s + " + INTERVAL " + wrapTerm(a[1].s, a[1].e) + " MONTH)", tTime, true
}

// oraLastDay writes LAST_DAY, which keeps the time of day in Oracle.
func oraLastDay(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	if _, simple := a[0].e.(*nameExpr); !simple {
		if _, lit := a[0].e.(*litExpr); !lit {
			g.refuse(c.line, "LAST_DAY of an expression: write the expression into a variable first")
			return "NULL", tTime, true
		}
	}
	if g.to == Postgres {
		return "(date_trunc('month', " + a[0].s + ") + INTERVAL '1 month' - INTERVAL '1 day' + (" + a[0].s + " - date_trunc('day', " + a[0].s + ")))", tTime, true
	}
	return "TIMESTAMP(LAST_DAY(" + a[0].s + "), TIME(" + a[0].s + "))", tTime, true
}

// oraUpdating writes UPDATING('COLUMN'), which is true when the UPDATE
// statement names the column.
func oraUpdating(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 || g.r.trigger == nil {
		return "", tUnknown, false
	}
	col, ok := literalText(a[0])
	if !ok {
		return "", tUnknown, false
	}
	g.note(c.line, "UPDATING(%s) is true in Oracle when the statement sets the column, even to the value it had; the converted code tests whether the value changed", g.stringLit(col))
	n := g.quote(strings.ToLower(col))
	if g.to == Postgres {
		return "(TG_OP = 'UPDATE' AND NEW." + n + " IS DISTINCT FROM OLD." + n + ")", tBool, true
	}
	if g.event != "UPDATE" {
		return "FALSE", tBool, true
	}
	return "(NOT (NEW." + n + " <=> OLD." + n + "))", tBool, true
}

func oraListagg(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	sep := "''"
	if len(a) == 2 {
		sep = a[1].s
		if sep == "NULL" {
			sep = "''"
		}
	}
	s := "string_agg(" + g.asText(a[0].e, a[0].s, a[0].t, "joined with LISTAGG") + ", " + sep
	if c.withinOrder != nil {
		s += " ORDER BY " + g.parts(c.withinOrder)
	}
	return s + ")", tStr, true
}
