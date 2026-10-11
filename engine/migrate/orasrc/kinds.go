package orasrc

import "strings"

// Kind says how a column's values are read from Oracle. Each kind has
// one text form, produced by SelectExpr, which every target converts
// into what it stores.
type Kind int

const (
	KindText Kind = iota
	KindNumber
	KindDate
	KindTimestamp
	KindTimestampTZ
	KindTimestampLTZ
	KindIntervalYM
	KindIntervalDS
	KindBinary
	KindRowid
	KindXML
	KindJSON
	KindBool
)

// Classify works out how values of an Oracle data type, named as the
// catalog names it, are read. It returns false for a type that cannot be
// migrated.
func Classify(dataType string) (Kind, bool) {
	t := dataType
	switch {
	case t == "VARCHAR2" || t == "NVARCHAR2" || t == "VARCHAR":
		return KindText, true
	case t == "CHAR" || t == "NCHAR":
		return KindText, true
	case t == "CLOB" || t == "NCLOB" || t == "LONG":
		return KindText, true
	case t == "NUMBER" || t == "FLOAT" || t == "BINARY_FLOAT" || t == "BINARY_DOUBLE":
		return KindNumber, true
	case t == "DATE":
		// An Oracle DATE carries a time of day.
		return KindDate, true
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH LOCAL TIME ZONE"):
		return KindTimestampLTZ, true
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH TIME ZONE"):
		return KindTimestampTZ, true
	case strings.HasPrefix(t, "TIMESTAMP"):
		return KindTimestamp, true
	case strings.HasPrefix(t, "INTERVAL YEAR"):
		return KindIntervalYM, true
	case strings.HasPrefix(t, "INTERVAL DAY"):
		return KindIntervalDS, true
	case t == "RAW" || t == "LONG RAW" || t == "BLOB":
		return KindBinary, true
	case t == "ROWID" || t == "UROWID":
		return KindRowid, true
	case t == "XMLTYPE":
		return KindXML, true
	case t == "JSON":
		return KindJSON, true
	case t == "BOOLEAN":
		return KindBool, true
	}
	return KindText, false
}
