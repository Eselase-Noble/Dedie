package codesql

import (
	"fmt"
	"strings"
)

// class is what kind of value an expression has, as far as the rewriting
// cares. Several rewrites depend on it: adding a number to a date is
// spelled differently from adding two numbers, and dividing two integers
// means something else in each database.
type class int

const (
	cUnknown class = iota
	cInt           // an integer type of the target database
	cNum           // an exact decimal number
	cFloat         // a binary floating point number
	cStr
	cDate   // a day without a time
	cTime   // a day and a time without a zone
	cTimeTZ // a moment
	cInterval
	cBool
	cBytes
	cRecord
	cNull // the literal NULL, which fits anything
	cOther
)

// typ is the class of a value and, for exact numbers, how many digits it
// has after the decimal point (-1 when that is not known).
type typ struct {
	class class
	scale int
}

var (
	tUnknown  = typ{cUnknown, -1}
	tInt      = typ{cInt, 0}
	tNum      = typ{cNum, -1}
	tWhole    = typ{cNum, 0}
	tFloat    = typ{cFloat, -1}
	tStr      = typ{cStr, -1}
	tDate     = typ{cDate, -1}
	tTime     = typ{cTime, -1}
	tTimeTZ   = typ{cTimeTZ, -1}
	tInterval = typ{cInterval, -1}
	tBool     = typ{cBool, -1}
	tNull     = typ{cNull, -1}
)

// numeric reports whether the value is a number of any kind.
func (t typ) numeric() bool { return t.class == cInt || t.class == cNum || t.class == cFloat }

// whole reports whether the value is known to have no fraction.
func (t typ) whole() bool { return t.class == cInt || (t.class == cNum && t.scale == 0) }

// temporal reports whether the value is a date or a time of any kind.
func (t typ) temporal() bool { return t.class == cDate || t.class == cTime || t.class == cTimeTZ }

func (t typ) known() bool { return t.class != cUnknown }

// dataType is a type as a routine writes it.
type dataType struct {
	name string // the words of the type in upper case, joined by single spaces
	args []int  // what is in the parentheses: a length, or a precision and a scale
	// star is NUMBER(*,s): no stated precision.
	star     bool
	unsigned bool
	// ref is the table and column of column%TYPE, or the table or cursor
	// of name%ROWTYPE.
	ref     []ident
	rowtype bool
	array   bool
	setOf   bool // PostgreSQL's SETOF and TABLE(...) results
	line    int
}

// String writes the type roughly as the source did, for messages.
func (d *dataType) String() string {
	if d == nil {
		return ""
	}
	if len(d.ref) > 0 {
		var parts []string
		for _, p := range d.ref {
			parts = append(parts, p.name)
		}
		if d.rowtype {
			return strings.Join(parts, ".") + "%ROWTYPE"
		}
		return strings.Join(parts, ".") + "%TYPE"
	}
	s := d.name
	if len(d.args) > 0 {
		var a []string
		for _, n := range d.args {
			a = append(a, fmt.Sprint(n))
		}
		s += "(" + strings.Join(a, ",") + ")"
	}
	if d.unsigned {
		s += " UNSIGNED"
	}
	if d.array {
		s += "[]"
	}
	return s
}

func (d *dataType) arg(i int) (int, bool) {
	if i < len(d.args) {
		return d.args[i], true
	}
	return 0, false
}

// mapped is a type translated for the target.
type mapped struct {
	text string
	typ  typ
	// note is set when the translation changes what the type can hold.
	note string
	// text types take a collation on a MySQL target.
	text8 bool
}

// The four functions below mirror the pipelines' column translations
// (orapg.translate, oramy.translate, mypg.pgType, pgmy.mysqlType), so a
// variable declared like a column gets the type that column got. They
// differ from those only where a routine can say something a column
// cannot: a parameter written without a length, or PL/SQL's own types.

// typeOraPG translates a PL/SQL type for PostgreSQL.
func typeOraPG(d *dataType) (mapped, bool) {
	n0, has0 := d.arg(0)
	n1, has1 := d.arg(1)
	switch d.name {
	case "VARCHAR2", "NVARCHAR2", "VARCHAR", "STRING", "CHARACTER VARYING", "CHAR VARYING":
		if has0 {
			return mapped{text: fmt.Sprintf("varchar(%d)", n0), typ: tStr}, true
		}
		return mapped{text: "text", typ: tStr}, true
	case "CHAR", "NCHAR", "CHARACTER":
		if has0 {
			return mapped{text: fmt.Sprintf("char(%d)", n0), typ: tStr}, true
		}
		return mapped{text: "char(1)", typ: tStr}, true
	case "CLOB", "NCLOB", "LONG":
		return mapped{text: "text", typ: tStr}, true
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		switch {
		case !has0 && !d.star:
			return mapped{text: "numeric", typ: tNum}, true
		case d.star:
			// NUMBER(*,s) has the full 38 digits.
			n0 = 38
		}
		switch {
		case n1 == 0 && n0 <= 4:
			return mapped{text: "smallint", typ: tInt}, true
		case n1 == 0 && n0 <= 9:
			return mapped{text: "integer", typ: tInt}, true
		case n1 == 0 && n0 <= 18:
			return mapped{text: "bigint", typ: tInt}, true
		case n1 < 0 || n1 > n0:
			return mapped{text: "numeric", typ: tNum}, true
		}
		return mapped{text: fmt.Sprintf("numeric(%d,%d)", n0, n1), typ: typ{cNum, n1}}, true
	case "INTEGER", "INT", "SMALLINT":
		// These are NUMBER(38) in Oracle, which is how the column
		// translation sees them.
		return mapped{text: "numeric(38,0)", typ: tWhole}, true
	case "PLS_INTEGER", "BINARY_INTEGER", "SIMPLE_INTEGER", "NATURAL", "NATURALN", "POSITIVE", "POSITIVEN", "SIGNTYPE":
		// 32-bit integers in both languages.
		return mapped{text: "integer", typ: tInt}, true
	case "FLOAT", "REAL", "DOUBLE PRECISION":
		if d.name == "FLOAT" && has0 && n0 <= 53 {
			return mapped{text: "double precision", typ: tFloat}, true
		}
		return mapped{text: "numeric", typ: tNum}, true
	case "BINARY_FLOAT":
		return mapped{text: "real", typ: tFloat}, true
	case "BINARY_DOUBLE":
		return mapped{text: "double precision", typ: tFloat}, true
	case "DATE":
		// An Oracle DATE carries a time of day.
		return mapped{text: "timestamp(0)", typ: tTime}, true
	case "TIMESTAMP":
		return mapped{text: fmt.Sprintf("timestamp(%d)", fraction(n0, has0)), typ: tTime}, true
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		m := mapped{text: fmt.Sprintf("timestamptz(%d)", fraction(n0, has0)), typ: tTimeTZ}
		if d.name == "TIMESTAMP WITH TIME ZONE" {
			m.note = "TIMESTAMP WITH TIME ZONE keeps the zone each value was entered in; PostgreSQL keeps the instant only"
		}
		return m, true
	case "INTERVAL YEAR TO MONTH":
		return mapped{text: "interval year to month", typ: tInterval}, true
	case "INTERVAL DAY TO SECOND":
		return mapped{text: "interval day to second", typ: tInterval}, true
	case "RAW", "LONG RAW", "BLOB":
		return mapped{text: "bytea", typ: typ{cBytes, -1}}, true
	case "BOOLEAN":
		return mapped{text: "boolean", typ: tBool}, true
	case "ROWID", "UROWID":
		return mapped{text: "text", typ: tStr, note: "a ROWID is stored as text and no longer points at a row"}, true
	case "JSON":
		return mapped{text: "jsonb", typ: typ{cOther, -1}}, true
	}
	_ = has1
	return mapped{}, false
}

// fraction is the number of fractional digits of a second a timestamp
// keeps on the target: what the source asked for, and 6 at most.
func fraction(n int, stated bool) int {
	if !stated || n > 6 {
		return 6
	}
	return n
}

// typeOraMy translates a PL/SQL type for MySQL.
func typeOraMy(d *dataType) (mapped, bool) {
	n0, has0 := d.arg(0)
	n1, _ := d.arg(1)
	switch d.name {
	case "VARCHAR2", "NVARCHAR2", "VARCHAR", "STRING", "CHARACTER VARYING", "CHAR VARYING":
		if !has0 || n0 > 16383 {
			return mapped{text: "text", typ: tStr, text8: true}, true
		}
		return mapped{text: fmt.Sprintf("varchar(%d)", n0), typ: tStr, text8: true}, true
	case "CHAR", "NCHAR", "CHARACTER":
		if !has0 {
			n0 = 1
		}
		if n0 > 255 {
			return mapped{text: fmt.Sprintf("varchar(%d)", n0), typ: tStr, text8: true}, true
		}
		return mapped{text: fmt.Sprintf("char(%d)", n0), typ: tStr, text8: true}, true
	case "CLOB", "NCLOB", "LONG":
		return mapped{text: "longtext", typ: tStr, text8: true}, true
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC", "INTEGER", "INT", "SMALLINT":
		if d.name == "INTEGER" || d.name == "INT" || d.name == "SMALLINT" {
			n0, n1, has0 = 38, 0, true
		}
		switch {
		case !has0 && !d.star:
			return mapped{text: "decimal(65,30)", typ: typ{cNum, 30},
				note: "NUMBER without a precision becomes DECIMAL(65,30), which rounds decimal places beyond 30"}, true
		case d.star:
			n0 = 38
		}
		switch {
		case n1 <= 0:
			digits := n0 - n1
			switch {
			case digits <= 2:
				return mapped{text: "tinyint", typ: tInt}, true
			case digits <= 4:
				return mapped{text: "smallint", typ: tInt}, true
			case digits <= 9:
				return mapped{text: "int", typ: tInt}, true
			case digits <= 18:
				return mapped{text: "bigint", typ: tInt}, true
			case digits <= 65:
				return mapped{text: fmt.Sprintf("decimal(%d,0)", digits), typ: tWhole}, true
			}
			return mapped{text: "double", typ: tFloat, note: "the NUMBER has more digits than MySQL's DECIMAL allows and becomes DOUBLE"}, true
		case n1 > 30:
			return mapped{text: "double", typ: tFloat, note: "the NUMBER has more decimal places than MySQL's DECIMAL allows and becomes DOUBLE"}, true
		}
		return mapped{text: fmt.Sprintf("decimal(%d,%d)", max(n0, n1), n1), typ: typ{cNum, n1}}, true
	case "PLS_INTEGER", "BINARY_INTEGER", "SIMPLE_INTEGER", "NATURAL", "NATURALN", "POSITIVE", "POSITIVEN", "SIGNTYPE":
		return mapped{text: "int", typ: tInt}, true
	case "FLOAT", "REAL", "DOUBLE PRECISION":
		m := mapped{text: "double", typ: tFloat}
		if !(d.name == "FLOAT" && has0 && n0 <= 53) {
			m.note = "FLOAT keeps up to 38 significant digits; it becomes DOUBLE, which keeps about 15"
		}
		return m, true
	case "BINARY_FLOAT":
		return mapped{text: "float", typ: tFloat}, true
	case "BINARY_DOUBLE":
		return mapped{text: "double", typ: tFloat}, true
	case "DATE":
		return mapped{text: "datetime", typ: tTime}, true
	case "TIMESTAMP":
		if f := fraction(n0, has0); f > 0 {
			return mapped{text: fmt.Sprintf("datetime(%d)", f), typ: tTime}, true
		}
		return mapped{text: "datetime", typ: tTime}, true
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		return mapped{text: "datetime(6)", typ: tTimeTZ,
			note: "a timestamp with a time zone becomes DATETIME(6), which the data migration fills with UTC"}, true
	case "RAW":
		if !has0 {
			n0 = 2000
		}
		return mapped{text: fmt.Sprintf("varbinary(%d)", n0), typ: typ{cBytes, -1}}, true
	case "LONG RAW", "BLOB":
		return mapped{text: "longblob", typ: typ{cBytes, -1}}, true
	case "BOOLEAN":
		return mapped{text: "tinyint(1)", typ: tBool}, true
	case "JSON":
		return mapped{text: "json", typ: typ{cOther, -1}}, true
	}
	return mapped{}, false
}

// sized writes a type with its arguments, if it has any.
func sized(base string, d *dataType) string {
	switch len(d.args) {
	case 1:
		return fmt.Sprintf("%s(%d)", base, d.args[0])
	case 2:
		return fmt.Sprintf("%s(%d,%d)", base, d.args[0], d.args[1])
	}
	return base
}

// typeMyPG translates a MySQL type for PostgreSQL.
func typeMyPG(d *dataType) (mapped, bool) {
	n1, has1 := d.arg(1)
	switch d.name {
	case "TINYINT", "BOOL", "BOOLEAN":
		// MySQL's BOOLEAN is TINYINT(1): a small number, not a truth value.
		return mapped{text: "smallint", typ: tInt}, true
	case "SMALLINT":
		if d.unsigned {
			return mapped{text: "integer", typ: tInt}, true
		}
		return mapped{text: "smallint", typ: tInt}, true
	case "MEDIUMINT":
		return mapped{text: "integer", typ: tInt}, true
	case "INT", "INTEGER":
		if d.unsigned {
			return mapped{text: "bigint", typ: tInt}, true
		}
		return mapped{text: "integer", typ: tInt}, true
	case "BIGINT":
		if d.unsigned {
			return mapped{text: "numeric(20,0)", typ: tWhole}, true
		}
		return mapped{text: "bigint", typ: tInt}, true
	case "DECIMAL", "NUMERIC", "DEC", "FIXED":
		// DECIMAL without a size is DECIMAL(10,0) in MySQL.
		if len(d.args) == 0 {
			return mapped{text: "numeric(10,0)", typ: tWhole}, true
		}
		t := typ{cNum, 0}
		if has1 {
			t.scale = n1
		}
		return mapped{text: sized("numeric", d), typ: t}, true
	case "FLOAT":
		return mapped{text: "real", typ: tFloat}, true
	case "DOUBLE", "DOUBLE PRECISION", "REAL":
		return mapped{text: "double precision", typ: tFloat}, true
	case "BIT":
		return mapped{text: "bigint", typ: tInt}, true
	case "YEAR":
		return mapped{text: "smallint", typ: tInt}, true
	case "CHAR", "VARCHAR", "CHARACTER", "NCHAR", "NVARCHAR", "CHARACTER VARYING":
		if len(d.args) == 0 && d.name != "VARCHAR" {
			return mapped{text: "varchar(1)", typ: tStr}, true
		}
		// CHAR pads with spaces in PostgreSQL but not in MySQL's output.
		return mapped{text: sized("varchar", d), typ: tStr}, true
	case "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT", "ENUM", "SET":
		return mapped{text: "text", typ: tStr}, true
	case "JSON":
		return mapped{text: "jsonb", typ: typ{cOther, -1}}, true
	case "DATE":
		return mapped{text: "date", typ: tDate}, true
	case "DATETIME":
		return mapped{text: sized("timestamp", d), typ: tTime}, true
	case "TIMESTAMP":
		// A MySQL TIMESTAMP is a point in time, stored in UTC.
		return mapped{text: sized("timestamptz", d), typ: tTimeTZ}, true
	case "TIME":
		return mapped{text: sized("time", d), typ: typ{cOther, -1}}, true
	case "BINARY", "VARBINARY", "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB":
		return mapped{text: "bytea", typ: typ{cBytes, -1}}, true
	}
	return mapped{}, false
}

// typePGMy translates a PostgreSQL type for MySQL.
func typePGMy(d *dataType) (mapped, bool) {
	n0, has0 := d.arg(0)
	n1, has1 := d.arg(1)
	switch d.name {
	case "SMALLINT", "INT2":
		return mapped{text: "SMALLINT", typ: tInt}, true
	case "INTEGER", "INT", "INT4":
		return mapped{text: "INT", typ: tInt}, true
	case "BIGINT", "INT8":
		return mapped{text: "BIGINT", typ: tInt}, true
	case "NUMERIC", "DECIMAL":
		// MySQL's DECIMAL stops at 65 digits, 30 of them after the point.
		if !has0 {
			return mapped{text: "DECIMAL(65,20)", typ: typ{cNum, 20},
				note: "numeric without a precision becomes DECIMAL(65,20), which rounds decimal places beyond 20"}, true
		}
		if !has1 {
			return mapped{text: fmt.Sprintf("DECIMAL(%d)", n0), typ: tWhole}, true
		}
		return mapped{text: fmt.Sprintf("DECIMAL(%d,%d)", n0, n1), typ: typ{cNum, n1}}, true
	case "REAL", "FLOAT4":
		return mapped{text: "FLOAT", typ: tFloat}, true
	case "DOUBLE PRECISION", "FLOAT8", "FLOAT":
		return mapped{text: "DOUBLE", typ: tFloat}, true
	case "BOOLEAN", "BOOL":
		return mapped{text: "TINYINT(1)", typ: tBool}, true
	case "VARCHAR", "CHARACTER VARYING":
		if !has0 {
			return mapped{text: "LONGTEXT", typ: tStr, text8: true}, true
		}
		return mapped{text: fmt.Sprintf("VARCHAR(%d)", n0), typ: tStr, text8: true}, true
	case "CHAR", "CHARACTER", "BPCHAR":
		if !has0 {
			n0 = 1
		}
		return mapped{text: fmt.Sprintf("CHAR(%d)", n0), typ: tStr, text8: true}, true
	case "TEXT", "NAME":
		return mapped{text: "LONGTEXT", typ: tStr, text8: true}, true
	case "BYTEA":
		return mapped{text: "LONGBLOB", typ: typ{cBytes, -1}}, true
	case "DATE":
		return mapped{text: "DATE", typ: tDate}, true
	case "TIMESTAMP", "TIMESTAMP WITHOUT TIME ZONE":
		return mapped{text: "DATETIME(6)", typ: tTime}, true
	case "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE":
		return mapped{text: "DATETIME(6)", typ: tTimeTZ,
			note: "a timestamp with time zone becomes DATETIME(6), which the data migration fills with UTC"}, true
	case "UUID":
		return mapped{text: "CHAR(36)", typ: tStr, text8: true}, true
	case "JSON", "JSONB":
		return mapped{text: "JSON", typ: typ{cOther, -1}}, true
	}
	return mapped{}, false
}

// mapType translates a type between two dialects.
func mapType(from, to Dialect, d *dataType) (mapped, bool) {
	if d.array || d.setOf {
		return mapped{}, false
	}
	switch {
	case from == Oracle && to == Postgres:
		return typeOraPG(d)
	case from == Oracle && to == MySQL:
		return typeOraMy(d)
	case from == MySQL && to == Postgres:
		return typeMyPG(d)
	case from == Postgres && to == MySQL:
		return typePGMy(d)
	}
	return mapped{}, false
}
