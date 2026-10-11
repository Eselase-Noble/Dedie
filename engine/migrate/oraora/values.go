package oraora

import (
	"database/sql"
	"fmt"
	"strings"

	go_ora "github.com/sijms/go-ora/v2"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/orasrc"
)

// Values travel as orasrc reads them: text in one fixed form per kind of
// column, large objects whole, binary values as bytes. The target turns
// the text back into the column's type with the conversion that mirrors
// the one the source printed it with, so a NUMBER arrives to its last
// digit, a TIMESTAMP(9) to the nanosecond and a time zone as the offset
// it was read with. The copy and the following of changes store rows
// with the same expressions, so a row arrives the same either way.

// largeObject reports whether a column is a CLOB, NCLOB or BLOB, for
// which an empty value and NULL are two different things.
func largeObject(c orasrc.Column) bool {
	return c.DataType == "CLOB" || c.DataType == "NCLOB" || c.DataType == "BLOB"
}

// emptyFlag is the expression that tells, on the source, an empty large
// object from NULL. The driver hands back both as NULL, so the copy
// reads this beside the column.
func emptyFlag(c orasrc.Column) string {
	col := oracle.Quote(c.Name)
	return "CASE WHEN " + col + " IS NOT NULL AND DBMS_LOB.GETLENGTH(" + col + ") = 0 THEN '1' END"
}

// binaryFloat reports whether a column is a BINARY_FLOAT or BINARY_DOUBLE.
func binaryFloat(c orasrc.Column) bool {
	return c.DataType == "BINARY_FLOAT" || c.DataType == "BINARY_DOUBLE"
}

// unprintable recognises what orasrc reads for an infinity or a NaN of
// those types: the number format it prints with has no way to write
// them, and Oracle fills the field with hash signs instead.
func unprintable(v any) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, "#")
}

// readExpr is how the copy reads a column: as orasrc does, except for
// the infinities and NaN, which are read by the names TO_BINARY_DOUBLE
// understands.
func readExpr(c orasrc.Column) string {
	if binaryFloat(c) {
		col := oracle.Quote(c.Name)
		return "CASE WHEN " + col + " IS NAN OR " + col + " IS INFINITE THEN TO_CHAR(" + col + ") ELSE " + orasrc.SelectExpr(c) + " END"
	}
	return orasrc.SelectExpr(c)
}

// storeExpr turns a bound value, named by ref, into the column's type.
// flag names the bound marker for an empty large object.
func storeExpr(c orasrc.Column, ref, flag string) string {
	switch c.Kind {
	case orasrc.KindNumber:
		switch c.DataType {
		case "BINARY_FLOAT":
			return "TO_BINARY_FLOAT(" + ref + ")"
		case "BINARY_DOUBLE":
			return "TO_BINARY_DOUBLE(" + ref + ")"
		}
		// Without a format TO_NUMBER reads plain and scientific notation
		// alike; a NUMBER too long to print in full comes in the latter.
		return "TO_NUMBER(" + ref + ")"
	case orasrc.KindDate:
		return "TO_DATE(" + ref + ", 'YYYY-MM-DD HH24:MI:SS')"
	case orasrc.KindTimestamp:
		return "TO_TIMESTAMP(" + ref + ", 'YYYY-MM-DD HH24:MI:SS.FF9')"
	case orasrc.KindTimestampTZ:
		return "TO_TIMESTAMP_TZ(" + ref + ", 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')"
	case orasrc.KindTimestampLTZ:
		// Read in UTC and marked so; the target stores it in its own
		// database time zone, as it does any value of this type.
		return "TO_TIMESTAMP_TZ(" + ref + ", 'YYYY-MM-DD HH24:MI:SS.FF9TZH')"
	case orasrc.KindIntervalYM:
		return "TO_YMINTERVAL(" + ref + ")"
	case orasrc.KindIntervalDS:
		return "TO_DSINTERVAL(" + ref + ")"
	case orasrc.KindBool:
		return "TO_BOOLEAN(" + ref + ")"
	case orasrc.KindRowid:
		if c.DataType == "ROWID" {
			return "CHARTOROWID(" + ref + ")"
		}
	case orasrc.KindXML:
		return "CASE WHEN " + ref + " IS NULL THEN NULL ELSE XMLTYPE(" + ref + ") END"
	case orasrc.KindJSON:
		return "CASE WHEN " + ref + " IS NULL THEN NULL ELSE JSON(" + ref + ") END"
	}
	switch c.DataType {
	case "CLOB", "NCLOB":
		return "CASE WHEN " + flag + " = '1' THEN EMPTY_CLOB() ELSE " + ref + " END"
	case "BLOB":
		return "CASE WHEN " + flag + " = '1' THEN EMPTY_BLOB() ELSE " + ref + " END"
	}
	return ref
}

// asText renders a value read from the source as a string.
func asText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

// bindValue wraps a value read from the source in the type the driver
// binds it with for the column: national text as such, so that it does
// not pass through the database character set, and large objects as
// locators, which have no limit on size.
func bindValue(c orasrc.Column, v any) any {
	switch {
	case c.DataType == "NCLOB":
		if v == nil {
			return go_ora.NClob{}
		}
		return go_ora.NClob{String: asText(v), Valid: true}
	case c.DataType == "CLOB" || c.Kind == orasrc.KindXML || c.Kind == orasrc.KindJSON:
		if v == nil {
			return go_ora.Clob{}
		}
		return go_ora.Clob{String: asText(v), Valid: true}
	case c.DataType == "BLOB":
		if b, ok := v.([]byte); ok && len(b) > 0 {
			return go_ora.Blob{Data: b, Valid: true}
		}
		return go_ora.Blob{}
	case c.Kind == orasrc.KindBinary:
		b, _ := v.([]byte)
		return b
	case c.DataType == "NVARCHAR2" || c.DataType == "NCHAR":
		// An empty string is NULL in Oracle.
		if v == nil {
			return go_ora.NVarChar("")
		}
		return go_ora.NVarChar(asText(v))
	}
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: asText(v), Valid: true}
}

// rowShape describes how a table's rows are bound: one parameter for
// each stored column, followed by one marker for each large object.
type rowShape struct {
	t *Table
	// lobs holds the positions in t.Cols of the large object columns.
	lobs []int
}

func shapeOf(t *Table) rowShape {
	s := rowShape{t: t}
	for _, i := range t.stored {
		if largeObject(t.Cols[i]) {
			s.lobs = append(s.lobs, i)
		}
	}
	return s
}

// params is the number of bound parameters of a row.
func (s rowShape) params() int { return len(s.t.stored) + len(s.lobs) }

// selectList is what the copy reads from the source, in the order the
// parameters are bound.
func (s rowShape) selectList() string {
	exprs := make([]string, 0, s.params())
	for _, i := range s.t.stored {
		exprs = append(exprs, readExpr(s.t.Cols[i]))
	}
	for _, i := range s.lobs {
		exprs = append(exprs, emptyFlag(s.t.Cols[i]))
	}
	return strings.Join(exprs, ", ")
}

// bound is the inline view, called s, that gives the parameters names,
// so that an expression can use one more than once. stores holds, for
// the stored column at each position, the expression that stores it.
func (s rowShape) bound() (view string, stores map[int]string) {
	flags := map[int]string{}
	var binds []string
	for n, i := range s.t.stored {
		binds = append(binds, fmt.Sprintf(":%d c%d", n+1, i))
	}
	for n, i := range s.lobs {
		flags[i] = fmt.Sprintf("s.f%d", i)
		binds = append(binds, fmt.Sprintf(":%d f%d", len(s.t.stored)+n+1, i))
	}
	stores = map[int]string{}
	for _, i := range s.t.stored {
		stores[i] = storeExpr(s.t.Cols[i], fmt.Sprintf("s.c%d", i), flags[i])
	}
	return "(SELECT " + strings.Join(binds, ", ") + " FROM dual) s", stores
}

// insertSQL stores new rows; the copy uses it.
func (s rowShape) insertSQL() string {
	view, stores := s.bound()
	cols, exprs := make([]string, len(s.t.stored)), make([]string, len(s.t.stored))
	for n, i := range s.t.stored {
		cols[n], exprs[n] = oracle.Quote(s.t.Cols[i].Name), stores[i]
	}
	return "INSERT INTO " + s.t.QName() + " (" + strings.Join(cols, ", ") + ") SELECT " + strings.Join(exprs, ", ") + " FROM " + view
}

// mergeSQL stores rows whole by primary key: a row that is there is
// overwritten, one that is not is inserted.
func (s rowShape) mergeSQL() string {
	view, stores := s.bound()
	isKey := map[int]bool{}
	var on, sets, cols, exprs []string
	for _, i := range s.t.PK {
		isKey[i] = true
		on = append(on, "t."+oracle.Quote(s.t.Cols[i].Name)+" = "+stores[i])
	}
	for _, i := range s.t.stored {
		col := oracle.Quote(s.t.Cols[i].Name)
		cols, exprs = append(cols, col), append(exprs, stores[i])
		if !isKey[i] {
			sets = append(sets, "t."+col+" = "+stores[i])
		}
	}
	sql := "MERGE INTO " + s.t.QName() + " t USING " + view + " ON (" + strings.Join(on, " AND ") + ")"
	// A table that is all key has nothing to overwrite.
	if len(sets) > 0 {
		sql += " WHEN MATCHED THEN UPDATE SET " + strings.Join(sets, ", ")
	}
	return sql + " WHEN NOT MATCHED THEN INSERT (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(exprs, ", ") + ")"
}

// rowBatch gathers rows to be sent to the target in one round trip. The
// driver binds a slice per parameter, each of one type.
type rowBatch struct {
	shape rowShape
	// values holds, per parameter, the values of the rows gathered so far.
	values [][]any
	rows   int
	bytes  int
}

func newBatch(shape rowShape) *rowBatch {
	return &rowBatch{shape: shape, values: make([][]any, shape.params())}
}

// add takes one row: the values of the stored columns in order, as read
// from the source, and for each large object whether it is empty rather
// than NULL.
func (b *rowBatch) add(stored []any, empty []bool) {
	for n, i := range b.shape.t.stored {
		v := stored[n]
		switch x := v.(type) {
		case string:
			b.bytes += len(x)
		case []byte:
			b.bytes += len(x)
		}
		b.values[n] = append(b.values[n], bindValue(b.shape.t.Cols[i], v))
	}
	for n := range b.shape.lobs {
		flag := sql.NullString{}
		if empty[n] {
			flag = sql.NullString{String: "1", Valid: true}
		}
		b.values[len(b.shape.t.stored)+n] = append(b.values[len(b.shape.t.stored)+n], flag)
	}
	b.rows++
}

// args returns the parameters for the driver: typed slices when there is
// more than one row, plain values for a single one.
func (b *rowBatch) args() []any {
	out := make([]any, len(b.values))
	for n, col := range b.values {
		if b.rows == 1 {
			out[n] = col[0]
			continue
		}
		out[n] = typedSlice(col)
	}
	return out
}

func (b *rowBatch) reset() {
	for n := range b.values {
		b.values[n] = nil
	}
	b.rows, b.bytes = 0, 0
}

// typedSlice turns values that are all of one type into a slice of that
// type, which is what the driver takes for an array of parameters.
func typedSlice(col []any) any {
	switch col[0].(type) {
	case sql.NullString:
		return convert[sql.NullString](col)
	case go_ora.NVarChar:
		return convert[go_ora.NVarChar](col)
	case go_ora.Clob:
		return convert[go_ora.Clob](col)
	case go_ora.NClob:
		return convert[go_ora.NClob](col)
	case go_ora.Blob:
		return convert[go_ora.Blob](col)
	case []byte:
		return convert[[]byte](col)
	}
	return col
}

func convert[T any](col []any) []T {
	out := make([]T, len(col))
	for i, v := range col {
		out[i], _ = v.(T)
	}
	return out
}
