package codesql

import (
	"strings"
)

// postgresMy are the built-in functions of PostgreSQL written for MySQL.
var postgresMy map[string]fnRule

func init() {
	utc := func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 0 {
			return "", tUnknown, false
		}
		return g.specialPostgres(&nameExpr{parts: []ident{{name: "CURRENT_TIMESTAMP"}}, line: c.line}, "CURRENT_TIMESTAMP")
	}
	postgresMy = map[string]fnRule{
		"COALESCE":              sameArg("COALESCE"),
		"NULLIF":                sameArg("NULLIF", 2),
		"GREATEST":              nullsDiffer("GREATEST"),
		"LEAST":                 nullsDiffer("LEAST"),
		"UPPER":                 pgText("UPPER", 1),
		"LOWER":                 pgText("LOWER", 1),
		"REVERSE":               pgText("REVERSE", 1),
		"REPEAT":                pgText("REPEAT", 2),
		"REPLACE":               pgText("REPLACE", 3),
		"LENGTH":                pgLength("CHAR_LENGTH"),
		"CHAR_LENGTH":           pgLength("CHAR_LENGTH"),
		"CHARACTER_LENGTH":      pgLength("CHAR_LENGTH"),
		"OCTET_LENGTH":          pgLength("LENGTH"),
		"SUBSTR":                pgSubstring,
		"SUBSTRING":             pgSubstring,
		"LEFT":                  pgLeft("LEFT"),
		"RIGHT":                 pgLeft("RIGHT"),
		"TRIM":                  pgTrim("TRIM", "BOTH"),
		"BTRIM":                 pgTrim("TRIM", "BOTH"),
		"LTRIM":                 pgTrim("LTRIM", "LEADING"),
		"RTRIM":                 pgTrim("RTRIM", "TRAILING"),
		"LPAD":                  pgPad("LPAD"),
		"RPAD":                  pgPad("RPAD"),
		"STRPOS":                pgStrpos(false),
		"POSITION":              pgStrpos(true),
		"CONCAT":                pgConcat,
		"CONCAT_WS":             pgConcatWS,
		"ROUND":                 pgRound,
		"TRUNC":                 pgTrunc,
		"FLOOR":                 pgWhole("FLOOR"),
		"CEIL":                  pgWhole("CEILING"),
		"CEILING":               pgWhole("CEILING"),
		"ABS":                   sameArg("ABS", 1),
		"SIGN":                  same("SIGN", tInt, 1),
		"SQRT":                  same("SQRT", tFloat, 1),
		"POWER":                 same("POW", tFloat, 2),
		"POW":                   same("POW", tFloat, 2),
		"MOD":                   pgMod,
		"COUNT":                 aggregate("COUNT"),
		"SUM":                   aggregate("SUM"),
		"MIN":                   aggregate("MIN"),
		"MAX":                   aggregate("MAX"),
		"AVG":                   average,
		"NOW":                   utc,
		"TRANSACTION_TIMESTAMP": utc,
		"STATEMENT_TIMESTAMP":   utc,
		"CLOCK_TIMESTAMP":       utc,
		"DATE_TRUNC":            pgDateTrunc,
		"DATE_PART":             pgDatePart,
		"TO_CHAR":               pgToChar,
	}
}

func pgText(name string, n int) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != n {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "given to "+name)
		return name + "(" + joinArgs(a) + ")", tStr, true
	}
}

func pgLength(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		return name + "(" + g.asText(a[0].e, a[0].s, a[0].t, "measured") + ")", tInt, true
	}
}

// pgSubstring writes substr. PostgreSQL counts a start below 1 as
// positions before the text, which shortens the result; MySQL gives
// nothing for 0 and counts a negative start from the end.
func pgSubstring(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 && len(a) != 3 {
		return "", tUnknown, false
	}
	a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "cut with substr")
	if !positive(a[1]) {
		g.note(c.line, "substr starts at %s; if that can be 0 or negative, PostgreSQL counts positions before the text and MySQL gives nothing or counts from the end", describeExpr(a[1].e))
	}
	if len(a) == 3 {
		if n, ok := intLit(a[2].e); !ok || n < 0 {
			g.note(c.line, "substr takes %s characters; if that can be negative, PostgreSQL raises an error and MySQL gives nothing", describeExpr(a[2].e))
		}
	}
	return "SUBSTRING(" + joinArgs(a) + ")", tStr, true
}

func pgLeft(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "cut with "+strings.ToLower(name))
		if n, ok := intLit(a[1].e); !ok || n < 0 {
			g.note(c.line, "%s takes %s characters; if that can be negative, PostgreSQL gives all but that many and MySQL gives nothing", strings.ToLower(name), describeExpr(a[1].e))
		}
		return name + "(" + joinArgs(a) + ")", tStr, true
	}
}

func pgTrim(name, side string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		switch len(a) {
		case 1:
			return name + "(" + a[0].s + ")", tStr, true
		case 2:
			// PostgreSQL removes any of a set of characters, MySQL a
			// string; the two agree for a single character.
			if text, ok := literalText(a[1]); ok && len([]rune(text)) == 1 {
				return "TRIM(" + side + " " + a[1].s + " FROM " + a[0].s + ")", tStr, true
			}
		}
		return "", tUnknown, false
	}
}

func pgPad(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 && len(a) != 3 {
			return "", tUnknown, false
		}
		a[0].s = g.asText(a[0].e, a[0].s, a[0].t, "padded")
		if len(a) == 2 {
			a = append(a, argv{s: "' '"})
		}
		return name + "(" + joinArgs(a) + ")", tStr, true
	}
}

// pgStrpos writes strpos(text, part) and POSITION(part IN text).
func pgStrpos(partFirst bool) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 2 {
			return "", tUnknown, false
		}
		if !partFirst {
			a[0], a[1] = a[1], a[0]
		}
		return "LOCATE(" + a[0].s + ", " + a[1].s + ")", tInt, true
	}
}

// pgConcat writes concat(), which skips NULL arguments, as MySQL's
// CONCAT_WS does and its CONCAT does not.
func pgConcat(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) == 0 {
		return "", tUnknown, false
	}
	parts := []string{"''"}
	for _, arg := range a {
		parts = append(parts, g.asText(arg.e, arg.s, arg.t, "joined to text"))
	}
	return "CONCAT_WS(" + strings.Join(parts, ", ") + ")", tStr, true
}

func pgConcatWS(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) < 2 {
		return "", tUnknown, false
	}
	parts := []string{a[0].s}
	for _, arg := range a[1:] {
		parts = append(parts, g.asText(arg.e, arg.s, arg.t, "joined to text"))
	}
	return "CONCAT_WS(" + strings.Join(parts, ", ") + ")", tStr, true
}

func pgRound(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	switch {
	case a[0].t.class == cFloat:
		g.note(c.line, "round of a floating point number: PostgreSQL rounds a half to the even neighbour, MySQL away from zero")
	case !a[0].t.known():
		g.note(c.line, "round is applied to %s, whose type is not known here; if it is a floating point number, PostgreSQL rounds a half to the even neighbour and MySQL away from zero", describeExpr(a[0].e))
	}
	t := tWhole
	if a[0].t.class == cInt {
		t = tInt
	}
	if len(a) == 2 {
		t = tNum
		if n, ok := intLit(a[1].e); ok && n >= 0 {
			t = typ{cNum, n}
		}
	}
	return "ROUND(" + joinArgs(a) + ")", t, true
}

func pgTrunc(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 && len(a) != 2 {
		return "", tUnknown, false
	}
	if a[0].t.temporal() {
		return "", tUnknown, false
	}
	if len(a) == 1 {
		return "TRUNCATE(" + a[0].s + ", 0)", tWhole, true
	}
	t := tNum
	if n, ok := intLit(a[1].e); ok && n >= 0 {
		t = typ{cNum, n}
	}
	return "TRUNCATE(" + joinArgs(a) + ")", t, true
}

func pgWhole(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) != 1 {
			return "", tUnknown, false
		}
		return name + "(" + a[0].s + ")", tWhole, true
	}
}

func pgMod(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	s, t := g.division(&binExpr{op: "%", l: a[0].e, r: a[1].e, line: c.line})
	return "(" + s + ")", t, true
}

// pgDateTrunc writes date_trunc for the units MySQL can reach with its
// own functions. The result stays a date and time, as in PostgreSQL.
func pgDateTrunc(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	unit, ok := literalText(a[0])
	if !ok {
		return "", tUnknown, false
	}
	if a[1].t.class == cTimeTZ {
		g.note(c.line, "date_trunc of a timestamp with time zone cuts at the boundary in the session's time zone; the converted code cuts the UTC value the data migration stored")
	}
	x := a[1].s
	switch strings.ToLower(unit) {
	case "day":
		return "CAST(DATE(" + x + ") AS DATETIME(6))", tTime, true
	case "month":
		return "CAST(DATE_FORMAT(" + x + ", '%Y-%m-01') AS DATETIME(6))", tTime, true
	case "year":
		return "CAST(DATE_FORMAT(" + x + ", '%Y-01-01') AS DATETIME(6))", tTime, true
	case "hour":
		return "CAST(DATE_FORMAT(" + x + ", '%Y-%m-%d %H:00:00') AS DATETIME(6))", tTime, true
	case "minute":
		return "CAST(DATE_FORMAT(" + x + ", '%Y-%m-%d %H:%i:00') AS DATETIME(6))", tTime, true
	case "second":
		return "CAST(DATE_FORMAT(" + x + ", '%Y-%m-%d %H:%i:%s') AS DATETIME(6))", tTime, true
	}
	g.refuse(c.line, "date_trunc to the %s has no MySQL form here", unit)
	return "NULL", tTime, true
}

func pgDatePart(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	field, ok := literalText(a[0])
	if !ok {
		return "", tUnknown, false
	}
	s, t := g.extract(&extractExpr{field: strings.ToUpper(field), x: a[1].e, line: c.line})
	return s, t, true
}

func pgToChar(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 2 {
		return "", tUnknown, false
	}
	format, ok := literalText(a[1])
	if !ok {
		g.refuse(c.line, "to_char with a format that is not a literal")
		return "NULL", tStr, true
	}
	if a[0].t.numeric() {
		g.refuse(c.line, "to_char of a number with a format: MySQL has no formats for numbers")
		return "NULL", tStr, true
	}
	if !a[0].t.temporal() {
		g.note(c.line, "to_char formats %s, whose type is not known here, with %s; it is taken to be a date", describeExpr(a[0].e), g.stringLit(format))
	}
	my, bad := pgToMySQLFormat(format)
	if bad != "" {
		g.refuse(c.line, "the date format element %s has no MySQL counterpart that gives the same text", bad)
		return "NULL", tStr, true
	}
	return "DATE_FORMAT(" + a[0].s + ", " + g.stringLit(my) + ")", tStr, true
}
