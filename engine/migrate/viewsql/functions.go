package viewsql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// fn writes a function call for the target. name is the function as the
// source calls it and args are its arguments, still untranslated.
type fn func(t *translator, name string, args [][]node) (string, error)

// splitArgs cuts an argument list at its commas.
func splitArgs(items []node) [][]node {
	if len(items) == 0 {
		return nil
	}
	var out [][]node
	start := 0
	for i := range items {
		if items[i].isOp(",") {
			out = append(out, items[start:i])
			start = i + 1
		}
	}
	return append(out, items[start:])
}

func (t *translator) exprs(args [][]node) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		text, err := t.expr(a)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, unsupported("it calls a function with an empty argument")
		}
		out[i] = text
	}
	return out, nil
}

func arity(name string) error {
	return unsupported("it calls %s() with a number of arguments this translator has no rule for", name)
}

// call translates a function call. Only functions on the list for the
// pair of dialects are carried over: a function the source and the target
// both have under one name is no evidence that they do the same thing.
func (t *translator) call(name string, items []node) (string, error) {
	f, ok := functions[t.o.From][t.o.To][name]
	if !ok {
		return "", unsupported("it calls %s(), which this translator has no rule for", name)
	}
	return f(t, name, splitArgs(items))
}

// as keeps a call as it is under the target's name for the function. max
// is -1 for any number of arguments.
func as(target string, min, max int) fn {
	return func(t *translator, name string, args [][]node) (string, error) {
		if len(args) < min || (max >= 0 && len(args) > max) {
			return "", arity(name)
		}
		list, err := t.exprs(args)
		if err != nil {
			return "", err
		}
		return target + "(" + strings.Join(list, ", ") + ")", nil
	}
}

// shape writes the arguments into a template, which refers to them as
// %[1]s, %[2]s and so on.
func shape(n int, format string) fn {
	return func(t *translator, name string, args [][]node) (string, error) {
		if len(args) != n {
			return "", arity(name)
		}
		list, err := t.exprs(args)
		if err != nil {
			return "", err
		}
		values := make([]any, n)
		for i, v := range list {
			values[i] = v
		}
		return fmt.Sprintf(format, values...), nil
	}
}

var positiveRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// positive keeps a call only when the arguments at the given positions
// are whole numbers above zero, written out. SUBSTR, LEFT and RIGHT agree
// between the dialects for those and for nothing else: a zero or negative
// position counts from the end in one database and is an error or an
// empty string in another.
func positive(target string, min, max int, positions ...int) fn {
	keep := as(target, min, max)
	return func(t *translator, name string, args [][]node) (string, error) {
		for _, p := range positions {
			if p < len(args) && !(len(args[p]) == 1 && args[p][0].isNumber() && positiveRe.MatchString(args[p][0].tok.text)) {
				return "", unsupported("it calls %s() with a position or length that is not a plain positive number", name)
			}
		}
		return keep(t, name, args)
	}
}

// trim keeps the one-argument form. TRIM(LEADING 'x' FROM y) and its
// relatives are left alone.
func trim(target string) fn {
	keep := as(target, 1, 1)
	return func(t *translator, name string, args [][]node) (string, error) {
		for _, a := range args {
			for _, n := range a {
				if n.isWord("FROM") || n.isWord("BOTH") || n.isWord("LEADING") || n.isWord("TRAILING") {
					return "", unsupported("it calls TRIM() with LEADING, TRAILING or BOTH")
				}
			}
		}
		return keep(t, name, args)
	}
}

// extract keeps EXTRACT for the fields that are whole numbers everywhere.
func extract(t *translator, name string, args [][]node) (string, error) {
	if len(args) != 1 || len(args[0]) < 3 || args[0][0].open != 0 || !args[0][1].isWord("FROM") {
		return "", arity(name)
	}
	field := strings.ToUpper(args[0][0].tok.text)
	if field != "YEAR" && field != "MONTH" && field != "DAY" {
		return "", unsupported("it extracts %s from a date, which the databases do not count alike", field)
	}
	value, err := t.expr(args[0][2:])
	if err != nil {
		return "", err
	}
	return "EXTRACT(" + field + " FROM " + value + ")", nil
}

// datePart translates PostgreSQL's older spelling of EXTRACT.
func datePart(t *translator, name string, args [][]node) (string, error) {
	if len(args) != 2 || len(args[0]) == 0 || !args[0][0].isString() {
		return "", arity(name)
	}
	field := strings.ToUpper(args[0][0].tok.text)
	if field != "YEAR" && field != "MONTH" && field != "DAY" {
		return "", unsupported("it extracts %s from a date, which the databases do not count alike", field)
	}
	value, err := t.expr(args[1])
	if err != nil {
		return "", err
	}
	return "EXTRACT(" + field + " FROM " + value + ")", nil
}

// fixed stands for a function without arguments.
func fixed(text string) fn {
	return func(t *translator, name string, args [][]node) (string, error) {
		if len(args) != 0 {
			return "", arity(name)
		}
		return text, nil
	}
}

// decode translates Oracle's DECODE(value, search, result, ..., default).
// DECODE counts NULL as equal to NULL, so the comparison has to be one
// that does; same is the target's, with two places for the operands.
func decode(same string) fn {
	return func(t *translator, name string, args [][]node) (string, error) {
		if len(args) < 3 {
			return "", arity(name)
		}
		list, err := t.exprs(args)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("CASE")
		i := 1
		for ; i+1 < len(list); i += 2 {
			b.WriteString(" WHEN " + fmt.Sprintf(same, list[0], list[i]) + " THEN " + list[i+1])
		}
		if i < len(list) {
			b.WriteString(" ELSE " + list[i])
		}
		b.WriteString(" END")
		return b.String(), nil
	}
}

// concatToPostgres translates MySQL's CONCAT, which is NULL as soon as one
// argument is. PostgreSQL's function of that name skips NULLs; its ||
// operator does what MySQL's function does.
func concatToPostgres(t *translator, name string, args [][]node) (string, error) {
	if len(args) == 0 {
		return "", arity(name)
	}
	list, err := t.exprs(args)
	if err != nil {
		return "", err
	}
	for i, v := range list {
		list[i] = "(" + v + ")::text"
	}
	return "(" + strings.Join(list, " || ") + ")", nil
}

// concatToMySQL translates PostgreSQL's concat, which skips NULLs, as
// MySQL's CONCAT_WS does.
func concatToMySQL(t *translator, name string, args [][]node) (string, error) {
	if len(args) == 0 {
		return "", arity(name)
	}
	list, err := t.exprs(args)
	if err != nil {
		return "", err
	}
	return "CONCAT_WS('', " + strings.Join(list, ", ") + ")", nil
}

// truncate translates TRUNC for a number, whose second argument MySQL
// insists on. Applied to a date the result is not a date on the target,
// which the verification of the view then reports.
func truncate(t *translator, name string, args [][]node) (string, error) {
	if len(args) == 1 {
		args = append(args, []node{{tok: token{tNumber, "0"}}})
	}
	return as("TRUNCATE", 2, 2)(t, name, args)
}

// castOperands splits CAST(value AS type) at its AS.
func castOperands(name string, args [][]node) (value, typ []node, err error) {
	if len(args) != 1 {
		return nil, nil, arity(name)
	}
	for i := len(args[0]) - 1; i > 0; i-- {
		if args[0][i].isWord("AS") {
			return args[0][:i], args[0][i+1:], nil
		}
	}
	return nil, nil, arity(name)
}

// typeWords reads a type name of MySQL or Oracle: its words, and the
// numbers in its brackets.
func typeWords(nodes []node) (words []string, sizes []int, err error) {
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.open == 0 && n.tok.kind == tWord:
			words = append(words, strings.ToUpper(n.tok.text))
		case n.isGroup('(') && sizes == nil:
			if sizes, err = typeSizes(n); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, unsupported("it casts to a type this translator has no rule for")
		}
	}
	if len(words) == 0 {
		return nil, nil, unsupported("it casts to a type this translator has no rule for")
	}
	return words, sizes, nil
}

// typeSizes reads the (10) or (12, 2) of a type.
func typeSizes(group *node) ([]int, error) {
	sizes := []int{}
	for i, item := range group.items {
		if i%2 == 1 && item.isOp(",") {
			continue
		}
		v, err := strconv.Atoi(item.tok.text)
		if i%2 == 1 || !item.isNumber() || err != nil {
			return nil, unsupported("it casts to a type this translator has no rule for")
		}
		sizes = append(sizes, v)
	}
	return sizes, nil
}

// castFromMySQL translates CAST(value AS type) for PostgreSQL.
func castFromMySQL(t *translator, name string, args [][]node) (string, error) {
	valueNodes, typeNodes, err := castOperands(name, args)
	if err != nil {
		return "", err
	}
	value, err := t.expr(valueNodes)
	if err != nil {
		return "", err
	}
	words, sizes, err := typeWords(typeNodes)
	if err != nil {
		return "", err
	}
	all := strings.Join(words, " ")
	// MySQL adds the character set of the session to a cast to CHAR.
	for _, suffix := range []string{" CHARSET UTF8MB4", " CHARACTER SET UTF8MB4", " CHARSET UTF8MB3", " CHARACTER SET UTF8MB3", " CHARSET UTF8", " CHARACTER SET UTF8"} {
		if strings.HasPrefix(all, "CHAR") {
			all = strings.TrimSuffix(all, suffix)
		}
	}
	var typ string
	switch {
	case all == "SIGNED" || all == "SIGNED INTEGER":
		typ = "bigint"
	case all == "CHAR" && len(sizes) == 0:
		typ = "text"
	case all == "CHAR" && len(sizes) == 1:
		typ = fmt.Sprintf("varchar(%d)", sizes[0])
	case all == "DECIMAL" && len(sizes) == 0:
		typ = "numeric(10,0)"
	case all == "DECIMAL" && len(sizes) == 1:
		typ = fmt.Sprintf("numeric(%d,0)", sizes[0])
	case all == "DECIMAL" && len(sizes) == 2:
		typ = fmt.Sprintf("numeric(%d,%d)", sizes[0], sizes[1])
	case all == "DATE" && len(sizes) == 0:
		typ = "date"
	case all == "DATETIME" && len(sizes) == 0:
		typ = "timestamp(0)"
	case all == "DATETIME" && len(sizes) == 1:
		typ = fmt.Sprintf("timestamp(%d)", sizes[0])
	case all == "DOUBLE" && len(sizes) == 0:
		typ = "double precision"
	default:
		return "", unsupported("it casts to %s", all)
	}
	return "CAST(" + value + " AS " + typ + ")", nil
}

// castFromOracle translates CAST(value AS type) for the numeric types.
// A cast to or from text or a date depends on the session's formats in
// Oracle and is left alone.
func castFromOracle(t *translator, name string, args [][]node) (string, error) {
	valueNodes, typeNodes, err := castOperands(name, args)
	if err != nil {
		return "", err
	}
	value, err := t.expr(valueNodes)
	if err != nil {
		return "", err
	}
	words, sizes, err := typeWords(typeNodes)
	if err != nil {
		return "", err
	}
	all := strings.Join(words, " ")
	precision, scale := 0, 0
	switch {
	case (all == "INTEGER" || all == "INT") && len(sizes) == 0:
		precision = 38
	case (all == "NUMBER" || all == "DECIMAL" || all == "NUMERIC") && len(sizes) == 1:
		precision = sizes[0]
	case (all == "NUMBER" || all == "DECIMAL" || all == "NUMERIC") && len(sizes) == 2:
		precision, scale = sizes[0], sizes[1]
	case all == "NUMBER" && len(sizes) == 0 && t.o.To == Postgres:
		return "CAST(" + value + " AS numeric)", nil
	default:
		return "", unsupported("it casts to %s", all)
	}
	if t.o.To == Postgres {
		return fmt.Sprintf("CAST(%s AS numeric(%d,%d))", value, precision, scale), nil
	}
	if precision > 65 || scale > 30 {
		return "", unsupported("it casts to a number with more digits than MySQL's DECIMAL has")
	}
	return fmt.Sprintf("CAST(%s AS DECIMAL(%d,%d))", value, precision, scale), nil
}

// castFromPostgres translates the CAST(value AS type) spelling, which
// PostgreSQL itself stores as value::type.
func castFromPostgres(t *translator, name string, args [][]node) (string, error) {
	valueNodes, typeNodes, err := castOperands(name, args)
	if err != nil {
		return "", err
	}
	value, err := t.expr(valueNodes)
	if err != nil {
		return "", err
	}
	typ, sizes, next, err := postgresType(typeNodes, 0)
	if err != nil {
		return "", err
	}
	if next != len(typeNodes) {
		return "", unsupported("it casts to a type this translator has no rule for")
	}
	p, err := castToMySQL(piece{kind: pPrimary, text: "(" + value + ")"}, typ, sizes)
	return p.text, err
}

func merge(maps ...map[string]fn) map[string]fn {
	out := map[string]fn{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// functions holds, for each pair of dialects, the functions that are
// carried over, by their name in the source. It is filled in by init,
// because the rules translate their arguments with the translator that
// looks them up here.
var functions map[Dialect]map[Dialect]map[string]fn

func init() {
	// shared are the functions that mean the same in all three dialects.
	// MySQL does not take a space between a function's name and its
	// bracket, so calls are always written without one.
	shared := map[string]fn{
		"COUNT": as("COUNT", 1, 1), "SUM": as("SUM", 1, 1), "MIN": as("MIN", 1, 1), "MAX": as("MAX", 1, 1), "AVG": as("AVG", 1, 1),
		"COALESCE": as("COALESCE", 1, -1), "NULLIF": as("NULLIF", 2, 2), "ABS": as("ABS", 1, 1), "SIGN": as("SIGN", 1, 1),
		"UPPER": as("UPPER", 1, 1), "LOWER": as("LOWER", 1, 1), "FLOOR": as("FLOOR", 1, 1), "CEIL": as("CEIL", 1, 1),
		"ROUND": as("ROUND", 1, 2), "MOD": as("MOD", 2, 2), "REPLACE": as("REPLACE", 3, 3),
		"LTRIM": as("LTRIM", 1, 1), "RTRIM": as("RTRIM", 1, 1), "TRIM": trim("TRIM"), "EXTRACT": extract,
	}

	functions = map[Dialect]map[Dialect]map[string]fn{
		MySQL: {Postgres: merge(shared, map[string]fn{
			"IFNULL": shape(2, "COALESCE(%[1]s, %[2]s)"),
			"IF":     shape(3, "CASE WHEN %[1]s THEN %[2]s ELSE %[3]s END"),
			"CONCAT": concatToPostgres, "CONCAT_WS": as("concat_ws", 2, -1),
			// MySQL's LENGTH counts bytes.
			"LENGTH": shape(1, "octet_length(%[1]s)"), "CHAR_LENGTH": as("char_length", 1, 1), "CHARACTER_LENGTH": as("char_length", 1, 1),
			"SUBSTRING": positive("substr", 2, 3, 1, 2), "SUBSTR": positive("substr", 2, 3, 1, 2), "MID": positive("substr", 3, 3, 1, 2),
			"LEFT": positive("left", 2, 2, 1), "RIGHT": positive("right", 2, 2, 1),
			"LCASE": as("LOWER", 1, 1), "UCASE": as("UPPER", 1, 1), "CEILING": as("CEIL", 1, 1), "TRUNCATE": as("trunc", 2, 2),
			// NOW() is a moment without a time zone or fractions of a second.
			"NOW": fixed("LOCALTIMESTAMP(0)"), "CURRENT_TIMESTAMP": fixed("LOCALTIMESTAMP(0)"), "LOCALTIMESTAMP": fixed("LOCALTIMESTAMP(0)"),
			"CURDATE": fixed("CURRENT_DATE"), "CURRENT_DATE": fixed("CURRENT_DATE"),
			"DATE": shape(1, "CAST(%[1]s AS date)"), "YEAR": shape(1, "EXTRACT(YEAR FROM %[1]s)"),
			"MONTH": shape(1, "EXTRACT(MONTH FROM %[1]s)"), "DAY": shape(1, "EXTRACT(DAY FROM %[1]s)"), "DAYOFMONTH": shape(1, "EXTRACT(DAY FROM %[1]s)"),
			"CAST": castFromMySQL,
		})},
		Postgres: {MySQL: merge(shared, map[string]fn{
			"CONCAT": concatToMySQL, "CONCAT_WS": as("CONCAT_WS", 2, -1),
			// PostgreSQL's length counts characters.
			"LENGTH": as("CHAR_LENGTH", 1, 1), "CHAR_LENGTH": as("CHAR_LENGTH", 1, 1), "CHARACTER_LENGTH": as("CHAR_LENGTH", 1, 1),
			"OCTET_LENGTH": as("LENGTH", 1, 1),
			"SUBSTR":       positive("SUBSTRING", 2, 3, 1, 2), "SUBSTRING": positive("SUBSTRING", 2, 3, 1, 2),
			"LEFT": positive("LEFT", 2, 2, 1), "RIGHT": positive("RIGHT", 2, 2, 1),
			"BTRIM": as("TRIM", 1, 1), "CEILING": as("CEIL", 1, 1), "TRUNC": truncate,
			"NOW": fixed("NOW(6)"), "TRANSACTION_TIMESTAMP": fixed("NOW(6)"),
			"DATE_PART": datePart, "CAST": castFromPostgres,
		})},
		Oracle: {
			Postgres: merge(shared, map[string]fn{
				"NVL":    shape(2, "COALESCE(%[1]s, %[2]s)"),
				"NVL2":   shape(3, "CASE WHEN %[1]s IS NOT NULL THEN %[2]s ELSE %[3]s END"),
				"DECODE": decode("(%s) IS NOT DISTINCT FROM (%s)"),
				"LENGTH": as("length", 1, 1), "SUBSTR": positive("substr", 2, 3, 1, 2), "TRUNC": as("trunc", 1, 2),
				"CAST": castFromOracle,
			}),
			MySQL: merge(shared, map[string]fn{
				"NVL":    shape(2, "COALESCE(%[1]s, %[2]s)"),
				"NVL2":   shape(3, "CASE WHEN %[1]s IS NOT NULL THEN %[2]s ELSE %[3]s END"),
				"DECODE": decode("(%s) <=> (%s)"),
				"LENGTH": as("CHAR_LENGTH", 1, 1), "SUBSTR": positive("SUBSTRING", 2, 3, 1, 2), "TRUNC": truncate,
				// Both return NULL as soon as one argument is NULL;
				// PostgreSQL's skip NULLs, so they are not on its list.
				"GREATEST": as("GREATEST", 2, -1), "LEAST": as("LEAST", 2, -1),
				"CAST": castFromOracle,
			}),
		},
	}
}
