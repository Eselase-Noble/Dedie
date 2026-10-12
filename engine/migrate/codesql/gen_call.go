package codesql

import (
	"strings"
)

// argv is an argument of a call, written for the target.
type argv struct {
	e expr
	s string
	t typ
}

// fnRule writes a call to a built-in function of the source for the
// target. It returns false when the function is used in a way the rule
// has no translation for, such as with more arguments than it knows.
type fnRule func(g *gen, c *callExpr, a []argv) (string, typ, bool)

// rules are the built-in functions of the source that have a rewrite.
func (g *gen) rules() map[string]fnRule {
	switch {
	case g.from == Oracle && g.to == Postgres:
		return oraclePG
	case g.from == Oracle:
		return oracleMy
	case g.from == MySQL:
		return mysqlPG
	}
	return postgresMy
}

// builtins are the functions of each source that the converter knows to
// be built in and has no rewrite for. A name in neither this list nor
// the rules is taken to be a routine of the user's.
var builtins = map[Dialect]map[string]bool{
	Oracle: words("MONTHS_BETWEEN NEXT_DAY REGEXP_LIKE REGEXP_SUBSTR REGEXP_REPLACE REGEXP_INSTR REGEXP_COUNT TO_CLOB TO_LOB " +
		"RAWTOHEX HEXTORAW SYS_CONTEXT USERENV NLS_UPPER NLS_LOWER NLS_INITCAP NLSSORT NUMTODSINTERVAL NUMTOYMINTERVAL " +
		"TO_DSINTERVAL TO_YMINTERVAL SYS_GUID DUMP VSIZE BITAND WIDTH_BUCKET LNNVL NANVL REMAINDER SOUNDEX TRANSLATE " +
		"XMLELEMENT XMLAGG XMLFOREST XMLTYPE EXTRACTVALUE JSON_VALUE JSON_QUERY JSON_OBJECT JSON_ARRAY JSON_EXISTS " +
		"TO_TIMESTAMP_TZ FROM_TZ NEW_TIME SESSIONTIMEZONE DBTIMEZONE TZ_OFFSET SYS_EXTRACT_UTC INSTRB SUBSTRB LENGTHB " +
		"ASCIISTR UNISTR COMPOSE DECOMPOSE CONVERT CARDINALITY ORA_HASH STANDARD_HASH EMPTY_CLOB EMPTY_BLOB BFILENAME " +
		"UID ROWIDTOCHAR CHARTOROWID TO_BINARY_DOUBLE TO_BINARY_FLOAT TO_NCHAR TO_MULTI_BYTE TO_SINGLE_BYTE RANK " +
		"DENSE_RANK ROW_NUMBER LAG LEAD FIRST_VALUE LAST_VALUE NTILE RATIO_TO_REPORT MEDIAN STDDEV VARIANCE CUME_DIST " +
		"PERCENT_RANK COLLECT CAST_TO_RAW SQLERRM LISTAGG INITCAP CHR LOG SIN COS TAN ASIN ACOS ATAN ATAN2 SINH COSH TANH"),
	MySQL: words("GROUP_CONCAT JSON_EXTRACT JSON_OBJECT JSON_ARRAY JSON_UNQUOTE JSON_SET JSON_CONTAINS FIND_IN_SET FIELD ELT " +
		"SUBSTRING_INDEX FORMAT INET_ATON INET_NTOA MD5 SHA1 SHA2 UUID UUID_SHORT RAND FOUND_ROWS LAST_INSERT_ID ROW_COUNT " +
		"CONNECTION_ID DATABASE SCHEMA USER SESSION_USER SYSTEM_USER VERSION SLEEP GET_LOCK RELEASE_LOCK BENCHMARK " +
		"UNIX_TIMESTAMP FROM_UNIXTIME STR_TO_DATE TIME_FORMAT TIMEDIFF ADDTIME SUBTIME MAKEDATE MAKETIME PERIOD_ADD " +
		"PERIOD_DIFF TO_DAYS FROM_DAYS TO_SECONDS SEC_TO_TIME TIME_TO_SEC WEEK WEEKOFYEAR YEARWEEK MONTHNAME DAYNAME " +
		"CONVERT_TZ UTC_DATE UTC_TIME CURTIME CURRENT_TIME TIME TIMESTAMP TIMESTAMPADD HEX UNHEX BIN OCT CONV ASCII ORD " +
		"CHAR SPACE STRCMP SOUNDEX INSERT LOCATE POSITION BIT_COUNT BIT_AND BIT_OR BIT_XOR CRC32 EXP LN LOG LOG2 " +
		"LOG10 PI SIN COS TAN ASIN ACOS ATAN ATAN2 COT DEGREES RADIANS STD STDDEV VARIANCE ROW_NUMBER RANK DENSE_RANK " +
		"LAG LEAD FIRST_VALUE LAST_VALUE NTILE ISNULL REGEXP_LIKE REGEXP_REPLACE REGEXP_SUBSTR REGEXP_INSTR " +
		"QUOTE EXPORT_SET MAKE_SET LOAD_FILE COMPRESS UNCOMPRESS AES_ENCRYPT AES_DECRYPT ANY_VALUE VALUES DEFAULT " +
		"CONVERT WEIGHT_STRING CHARSET COLLATION COERCIBILITY"),
	Postgres: words("NEXTVAL CURRVAL SETVAL LASTVAL STRING_AGG ARRAY_AGG ARRAY_LENGTH ARRAY_TO_STRING STRING_TO_ARRAY UNNEST " +
		"GENERATE_SERIES JSON_BUILD_OBJECT JSONB_BUILD_OBJECT JSON_AGG JSONB_AGG ROW_TO_JSON TO_JSON TO_JSONB FORMAT " +
		"SPLIT_PART REGEXP_REPLACE REGEXP_MATCH REGEXP_MATCHES REGEXP_SPLIT_TO_TABLE TRANSLATE INITCAP MD5 ENCODE DECODE " +
		"GEN_RANDOM_UUID UUID_GENERATE_V4 AGE MAKE_DATE MAKE_TIME MAKE_TIMESTAMP MAKE_INTERVAL JUSTIFY_DAYS " +
		"JUSTIFY_HOURS TO_TIMESTAMP TO_DATE TO_NUMBER TIMEOFDAY PG_SLEEP PG_ADVISORY_LOCK PG_ADVISORY_XACT_LOCK " +
		"PG_TRY_ADVISORY_LOCK CURRENT_SETTING SET_CONFIG CURRENT_SCHEMA CURRENT_DATABASE VERSION TXID_CURRENT " +
		"PG_BACKEND_PID QUOTE_IDENT QUOTE_LITERAL QUOTE_NULLABLE ROW_NUMBER RANK DENSE_RANK LAG LEAD FIRST_VALUE " +
		"LAST_VALUE NTILE BOOL_AND BOOL_OR EVERY STDDEV VARIANCE CHR ASCII OVERLAY STARTS_WITH NUM_NULLS NUM_NONNULLS " +
		"RANDOM SETSEED EXP LN LOG LOG10 PI SIN COS TAN ASIN ACOS ATAN ATAN2 DEGREES RADIANS DIV WIDTH_BUCKET SCALE " +
		"TRIM_SCALE TO_HEX BIT_LENGTH OCTET_LENGTH ISFINITE ROW XMLELEMENT TO_REGCLASS PG_TYPEOF " +
		"DATE_BIN ARRAY_APPEND ARRAY_POSITION ARRAY_REMOVE CARDINALITY JSONB_SET JSON_EXTRACT_PATH_TEXT " +
		"JSONB_EXTRACT_PATH_TEXT"),
}

// oraclePackages are the prefixes of the packages Oracle supplies. A
// call into one is never a routine of the user's.
var oraclePackages = []string{"DBMS_", "UTL_", "APEX_", "OWA_", "HTP", "HTF", "CTX_", "SDO_", "ORDSYS", "WPG_"}

// roundingLimit is how many decimal places a rounding may keep and still
// hide the difference in the digits two databases compute for a
// division. PostgreSQL carries at least 16 significant digits and Oracle
// 38; MySQL adds only 4 places to those of the dividend.
func (g *gen) roundingLimit() int {
	if g.from == MySQL || g.to == MySQL {
		return 2
	}
	return 10
}

var roundingFns = map[string]bool{"ROUND": true, "TRUNC": true, "TRUNCATE": true}

// call writes a function call.
func (g *gen) call(c *callExpr) (string, typ) {
	name := strings.ToUpper(c.name[len(c.name)-1].name)
	if len(c.name) == 1 {
		if rule := g.rules()[name]; rule != nil {
			rounds := false
			if roundingFns[name] {
				switch len(c.args) {
				case 1:
					rounds = true
				case 2:
					n, ok := intLit(c.args[1])
					rounds = ok && n <= g.roundingLimit()
				}
			}
			a := make([]argv, len(c.args))
			for i, arg := range c.args {
				if c.argNames[i] != "" {
					g.refuse(c.line, "the built-in function %s is called with a named argument", name)
				}
				if i == 0 && rounds {
					g.rounded++
				}
				a[i].e = arg
				a[i].s, a[i].t = g.value(arg)
				if i == 0 && rounds {
					g.rounded--
				}
			}
			s, t, ok := rule(g, c, a)
			if !ok {
				g.refuse(c.line, "this use of the built-in function %s (with %d arguments) has no translation", name, len(a))
				return "NULL", tUnknown
			}
			if c.over != nil {
				s += " OVER " + g.parts(c.over)
			}
			return s, t
		}
		if builtins[g.from][name] {
			g.refuse(c.line, "the built-in function %s has no translation", name)
			return "NULL", tUnknown
		}
	}
	if g.from == Oracle && len(c.name) > 1 {
		pkg := strings.ToUpper(c.name[len(c.name)-2].name)
		for _, p := range oraclePackages {
			if strings.HasPrefix(pkg, p) {
				g.refuse(c.line, "%s.%s belongs to a package Oracle supplies, which has no counterpart on the target", pkg, name)
				return "NULL", tUnknown
			}
		}
	}
	// What is left is a routine of the user's.
	if c.over != nil || c.distinct || c.star || c.withinOrder != nil {
		g.refuse(c.line, "the function %s is used as an aggregate or over a window, and the converter does not know it", name)
		return "NULL", tUnknown
	}
	var args []string
	for i, arg := range c.args {
		s, _ := g.value(arg)
		if c.argNames[i] != "" {
			if g.to == MySQL {
				g.refuse(c.line, "the call to %s names its arguments (%s => ...), which MySQL cannot do", name, c.argNames[i])
			}
			s = g.quote(strings.ToLower(c.argNames[i])) + " => " + s
		}
		args = append(args, s)
	}
	known := g.knownRoutine(c.name)
	if known == nil {
		switch {
		case g.from == Oracle && len(c.name) > 1:
			g.note(c.line, "%s is taken to be the function %s of the schema %s; if %s is a package, the call has to be rewritten", describeExpr(c), name, c.name[len(c.name)-2].name, c.name[len(c.name)-2].name)
		default:
			g.note(c.line, "%s is not a built-in function the converter knows nor a routine named in the migration; the call is written as it is", describeExpr(c))
		}
	}
	return g.objectName(c.name) + "(" + strings.Join(args, ", ") + ")", g.returnType(known)
}

// Helpers the rules of every route share.

func joinArgs(a []argv) string {
	parts := make([]string, len(a))
	for i := range a {
		parts[i] = a[i].s
	}
	return strings.Join(parts, ", ")
}

func arity(n int, allowed ...int) bool {
	for _, a := range allowed {
		if n == a {
			return true
		}
	}
	return false
}

// same keeps a function under a (possibly different) name, with a fixed
// result type.
func same(name string, result typ, allowed ...int) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if !arity(len(a), allowed...) {
			return "", tUnknown, false
		}
		return name + "(" + joinArgs(a) + ")", result, true
	}
}

// sameArg keeps a function whose result has the type of its first
// argument: ABS, MAX, COALESCE.
func sameArg(name string, allowed ...int) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(allowed) > 0 && !arity(len(a), allowed...) {
			return "", tUnknown, false
		}
		if len(a) == 0 {
			return "", tUnknown, false
		}
		t := tUnknown
		for _, arg := range a {
			if arg.t.known() && arg.t.class != cNull {
				t = arg.t
				break
			}
		}
		return name + "(" + joinArgs(a) + ")", t, true
	}
}

// aggregate writes COUNT, SUM, MIN and MAX, which are the same in all
// three databases.
func aggregate(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		inner := joinArgs(a)
		if c.star {
			inner = "*"
		}
		if c.distinct {
			inner = "DISTINCT " + inner
		}
		if len(a) > 1 || (len(a) == 0 && !c.star) {
			return "", tUnknown, false
		}
		t := tInt
		if name != "COUNT" && len(a) == 1 {
			t = a[0].t
			if name == "SUM" && t.class == cInt && g.to == Postgres {
				// The sum of integers is a bigint or a numeric in
				// PostgreSQL, never a type that divides as an integer
				// where the source's sum did not.
				t = tInt
			}
		}
		return name + "(" + inner + ")", t, true
	}
}

// average writes AVG. Its result is a division, with the same caveat
// about digits.
func average(g *gen, c *callExpr, a []argv) (string, typ, bool) {
	if len(a) != 1 {
		return "", tUnknown, false
	}
	if g.rounded == 0 && a[0].t.class != cFloat {
		g.note(c.line, "AVG keeps a different number of digits in %s than in %s; round the result if its last digits matter", g.to, g.from)
	}
	inner := a[0].s
	if c.distinct {
		inner = "DISTINCT " + inner
	}
	if a[0].t.class == cFloat {
		return "AVG(" + inner + ")", tFloat, true
	}
	return "AVG(" + inner + ")", tNum, true
}

// nullsDiffer notes GREATEST and LEAST, which PostgreSQL computes over
// the arguments that are not NULL while Oracle and MySQL give NULL as
// soon as one of them is.
func nullsDiffer(name string) fnRule {
	return func(g *gen, c *callExpr, a []argv) (string, typ, bool) {
		if len(a) < 2 {
			return "", tUnknown, false
		}
		other := g.from
		if g.from == Postgres {
			other = g.to
		}
		g.note(c.line, "%s gives NULL in %s when any argument is NULL; PostgreSQL ignores the NULL arguments", name, other)
		return sameArg(name)(g, c, a)
	}
}

// emptyIsNull wraps a string function for PostgreSQL or MySQL so that it
// gives NULL where Oracle would: Oracle has no empty string.
func emptyIsNull(s string) string { return "NULLIF(" + s + ", '')" }

// positive reports whether an argument is a whole-number literal of at
// least 1.
func positive(a argv) bool {
	n, ok := intLit(a.e)
	return ok && n >= 1
}

// literalText returns the value of a string literal argument.
func literalText(a argv) (string, bool) { return isLit(a.e, "string") }
