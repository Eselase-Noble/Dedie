package oramy

import (
	"fmt"
	"strings"

	"dedie/engine/migrate/orasrc"
)

// Limits of MySQL's types that decide a translation.
const (
	maxDecimalDigits = 65    // DECIMAL(65, ...)
	maxDecimalScale  = 30    // DECIMAL(..., 30)
	maxCharLength    = 255   // CHAR(255)
	maxVarcharLength = 16383 // VARCHAR(16383) is the longest that utf8mb4 allows
	// longBytes is what a text, blob or JSON column adds to its row: the
	// value itself is stored elsewhere.
	longBytes = 12
)

// myType is a MySQL column type and what the plan has to know about it.
type myType struct {
	DDL string
	// Bytes is the most room a value takes, in the row and in an index
	// key. Text is counted at four bytes a character, as MySQL does for
	// utf8mb4.
	Bytes int
	// Var marks the types stored with a length in front, which adds to
	// the row but not to an index key.
	Var bool
	// Long marks the text, blob and JSON types. They are kept outside
	// the row, cannot be indexed in full and take no literal default.
	Long bool
	// Scale is the number of decimal places of a DECIMAL.
	Scale int
	// Approx marks FLOAT and DOUBLE, whose values have no exact text.
	Approx bool
}

// integer reports whether the type is one of MySQL's integer types.
func (m myType) integer() bool {
	switch m.DDL {
	case "tinyint", "smallint", "int", "bigint":
		return true
	}
	return false
}

var (
	typeDouble   = myType{DDL: "double", Bytes: 8, Approx: true}
	typeBigint   = myType{DDL: "bigint", Bytes: 8}
	typeText     = myType{DDL: "text", Bytes: longBytes, Long: true}
	typeLongText = myType{DDL: "longtext", Bytes: longBytes, Long: true}
	typeBlob     = myType{DDL: "blob", Bytes: longBytes, Long: true}
	typeLongBlob = myType{DDL: "longblob", Bytes: longBytes, Long: true}
)

// decimalType is DECIMAL(digits, scale). MySQL packs nine digits into
// four bytes on each side of the point.
func decimalType(digits, scale int64) myType {
	left := []int{0, 1, 1, 2, 2, 3, 3, 4, 4}
	pack := func(n int64) int { return int(n/9)*4 + left[n%9] }
	return myType{DDL: fmt.Sprintf("decimal(%d,%d)", digits, scale), Bytes: pack(digits-scale) + pack(scale), Scale: int(scale)}
}

// wholeType is the smallest type that holds every whole number of the
// given count of decimal digits. It reports false when there are more
// digits than a DECIMAL has.
func wholeType(digits int64) (myType, bool) {
	switch {
	case digits <= 2:
		return myType{DDL: "tinyint", Bytes: 1}, true
	case digits <= 4:
		return myType{DDL: "smallint", Bytes: 2}, true
	case digits <= 9:
		return myType{DDL: "int", Bytes: 4}, true
	case digits <= 18:
		return typeBigint, true
	case digits <= maxDecimalDigits:
		return decimalType(digits, 0), true
	}
	return typeDouble, false
}

func varcharType(chars int64) myType {
	return myType{DDL: fmt.Sprintf("varchar(%d)", chars), Bytes: int(chars) * 4, Var: true}
}

// datetimeType is DATETIME with the given fractional digits of a second.
func datetimeType(fraction int64) myType {
	if fraction <= 0 {
		return myType{DDL: "datetime", Bytes: 5}
	}
	return myType{DDL: fmt.Sprintf("datetime(%d)", fraction), Bytes: 5 + int(fraction+1)/2}
}

// Notes for the translations that change what a column means. They are
// worded to stand after a list of the columns they apply to.
const (
	noteNumber = "NUMBER without a precision holds any number, and MySQL has no type that does. It becomes DECIMAL(65,30): exact for up to 35 digits before " +
		"the decimal point and 30 after it. Decimal places beyond 30 are rounded, and a larger number stops the migration. Values are shown with all 30 decimal places"
	noteWideNumber = "the NUMBER has more digits or decimal places than MySQL's DECIMAL allows (65 digits, 30 of them after the point); it becomes DOUBLE, which keeps about 15 significant digits"
	noteFloat      = "FLOAT keeps up to 38 significant digits; it becomes DOUBLE, which keeps about 15, so longer values are rounded"
	noteZone       = "TIMESTAMP WITH TIME ZONE keeps the zone each value was entered in; MySQL has no such type. The instant is stored as DATETIME(6) in UTC"
	noteLocalZone  = "TIMESTAMP WITH LOCAL TIME ZONE is shown in each session's own time zone; it becomes DATETIME(6) holding UTC, which reads the same in every session"
	noteInterval   = "MySQL has no interval type; the value is stored as ISO 8601 text, such as P2Y6M or P1DT2H3M4.5S"
	noteRowid      = "a ROWID is stored as text and no longer points at a row"
	noteXML        = "XML is stored as text; MySQL does not check that it is well formed"
	noteLongChar   = "the CHAR is longer than MySQL's CHAR can be (255 characters); it becomes VARCHAR and keeps its blank padding"
	noteLongText   = "the column is longer than MySQL's VARCHAR can be; it becomes TEXT, which does not enforce the length"
)

// translate picks the MySQL type for a column. rawLength is the declared
// length in bytes of a RAW column. translate returns a note when the
// translation changes meaning, and false when the type cannot be migrated.
func translate(c orasrc.Column, rawLength int64) (typ myType, note string, ok bool) {
	t := c.DataType
	lossy := ""
	if c.Scale.Valid && c.Scale.Int64 > 6 {
		lossy = fmt.Sprintf("; %s keeps %d fractional digits of a second and MySQL keeps 6, so values are rounded to the microsecond", t, c.Scale.Int64)
	}
	switch {
	case t == "VARCHAR2" || t == "NVARCHAR2" || t == "VARCHAR":
		// Oracle counts the length in bytes or in characters; either way
		// no value has more characters than CharLength.
		if c.CharLength <= 0 || c.CharLength > maxVarcharLength {
			return typeText, noteLongText, true
		}
		typ = varcharType(c.CharLength)
	case t == "CHAR" || t == "NCHAR":
		n := max(c.CharLength, 1)
		if n > maxCharLength {
			return varcharType(n), noteLongChar, true
		}
		typ = myType{DDL: fmt.Sprintf("char(%d)", n), Bytes: int(n) * 4}
	case t == "CLOB" || t == "NCLOB" || t == "LONG":
		typ = typeLongText
	case t == "NUMBER":
		p, s := c.Precision, c.Scale
		switch {
		case !p.Valid && !s.Valid:
			return decimalType(maxDecimalDigits, maxDecimalScale), noteNumber, true
		case !p.Valid:
			// NUMBER(*,s) has the full 38 digits. INTEGER and SMALLINT
			// are declared this way, with a scale of 0.
			p.Int64 = 38
		}
		switch {
		case s.Int64 <= 0:
			// A negative scale rounds to tens, hundreds and so on: whole
			// numbers with that many more digits.
			var fits bool
			if typ, fits = wholeType(p.Int64 - s.Int64); !fits {
				note = noteWideNumber
			}
		case s.Int64 > maxDecimalScale:
			typ, note = typeDouble, noteWideNumber
		default:
			// With a scale beyond the precision every digit lies after
			// the point: NUMBER(2,5) holds 0.00012.
			typ = decimalType(max(p.Int64, s.Int64), s.Int64)
		}
	case t == "FLOAT":
		// FLOAT is a decimal number with a binary precision; up to 53
		// bits fits a double exactly.
		typ = typeDouble
		if !c.Precision.Valid || c.Precision.Int64 > 53 {
			note = noteFloat
		}
	case t == "BINARY_FLOAT":
		typ = myType{DDL: "float", Bytes: 4, Approx: true}
	case t == "BINARY_DOUBLE":
		typ = typeDouble
	case t == "DATE":
		// An Oracle DATE carries a time of day.
		typ = datetimeType(0)
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH LOCAL TIME ZONE"):
		typ, note = datetimeType(6), noteLocalZone+lossy
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH TIME ZONE"):
		typ, note = datetimeType(6), noteZone+lossy
	case strings.HasPrefix(t, "TIMESTAMP"):
		n := int64(6)
		if c.Scale.Valid && c.Scale.Int64 < 6 {
			n = c.Scale.Int64
		}
		typ, note = datetimeType(n), strings.TrimPrefix(lossy, "; ")
	case strings.HasPrefix(t, "INTERVAL YEAR") || strings.HasPrefix(t, "INTERVAL DAY"):
		// The longest, P-999999999DT-23H-59M-59.999999999S, has 35 characters.
		typ, note = varcharType(64), noteInterval
	case t == "RAW":
		if rawLength <= 0 {
			// The longest a RAW can be declared without extended types.
			rawLength = 2000
		}
		typ = myType{DDL: fmt.Sprintf("varbinary(%d)", rawLength), Bytes: int(rawLength), Var: true}
	case t == "LONG RAW" || t == "BLOB":
		typ = typeLongBlob
	case t == "ROWID":
		typ, note = varcharType(18), noteRowid
	case t == "UROWID":
		// The text of a universal rowid has no useful upper bound.
		typ, note = typeText, noteRowid
	case t == "XMLTYPE":
		typ, note = typeLongText, noteXML
	case t == "JSON":
		typ = myType{DDL: "json", Bytes: longBytes, Long: true}
	case t == "BOOLEAN":
		typ = myType{DDL: "tinyint(1)", Bytes: 1}
	default:
		return myType{}, "", false
	}
	return typ, note, true
}
