package orasrc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// plainNumber prints a NUMBER without an exponent and with a zero before
// the decimal point, which is how other databases print a decimal.
const plainNumber = `'FM99999999999999999999999999999999999990.99999999999999999999'`

// TableSum is what validation compares between source and target: the
// row count and a sum over the primary keys. Sum is empty when only the
// rows were counted.
type TableSum struct {
	Count int64
	Sum   string
}

// Unicode reports whether the source stores text as UTF-8. Only then do
// the bytes of a text key hash alike on both sides.
func Unicode(ctx context.Context, src oracle.Queryer) bool {
	var charset string
	_ = src.QueryRowContext(ctx, `SELECT value FROM nls_database_parameters WHERE parameter = 'NLS_CHARACTERSET'`).Scan(&charset)
	return charset == "AL32UTF8" || charset == "UTF8"
}

// KeyExprs returns, for each primary key column, an Oracle expression
// that renders the value as text a target can reproduce in its own
// dialect. It reports false when some key column has no such form, in
// which case only counts can be compared.
//
// The forms a target has to match are: a NUMBER in plain decimal
// notation, without an exponent or trailing zeros, with a zero before a
// leading decimal point; a VARCHAR2 as it is; a CHAR without its
// trailing padding; a DATE as YYYY-MM-DD HH24:MI:SS. Text keys qualify
// only when utf8 says the source stores them as UTF-8 (see Unicode).
func KeyExprs(t *Table, utf8 bool) ([]string, bool) {
	if len(t.PK) == 0 {
		return nil, false
	}
	var exprs []string
	for _, i := range t.PK {
		c := t.Cols[i]
		col := oracle.Quote(c.Name)
		switch {
		case c.Kind == KindNumber && (c.DataType == "NUMBER"):
			exprs = append(exprs, "RTRIM(TO_CHAR("+col+", "+plainNumber+"), '.')")
		case c.Kind == KindText && c.DataType == "VARCHAR2" && utf8:
			exprs = append(exprs, col)
		case c.Kind == KindText && c.DataType == "CHAR" && utf8:
			exprs = append(exprs, "RTRIM("+col+")")
		case c.Kind == KindDate:
			exprs = append(exprs, "TO_CHAR("+col+", 'YYYY-MM-DD HH24:MI:SS')")
		default:
			return nil, false
		}
	}
	return exprs, true
}

// SourceSum computes a table's row count and, given the expressions of
// KeyExprs, the sum of a 60-bit slice of the MD5 of each row's primary
// key: the first fifteen hexadecimal digits of the hash of the key
// columns joined with '|', read as a number. The target computes the
// same in its own dialect. Whole rows cannot be compared this way,
// because two databases print many types differently, but equal counts
// with equal key sets rule out lost, duplicated and stray rows.
//
// With an SCN the table is read as of that moment; the caller decides
// what to do when that moment is gone (see UndoGone and Redefined). With
// nil keys only the rows are counted.
func SourceSum(ctx context.Context, src *sql.DB, t *Table, keys []string, scn int64) (TableSum, error) {
	var r TableSum
	from := t.Source()
	if scn > 0 {
		from += fmt.Sprintf(" AS OF SCN %d", scn)
	}
	if keys == nil {
		return r, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+from).Scan(&r.Count)
	}
	err := src.QueryRowContext(ctx, "SELECT COUNT(*), TO_CHAR(NVL(SUM(TO_NUMBER(SUBSTR(RAWTOHEX(STANDARD_HASH("+
		strings.Join(keys, " || '|' || ")+", 'MD5')), 1, 15), 'XXXXXXXXXXXXXXX')), 0), 'TM9') FROM "+from).Scan(&r.Count, &r.Sum)
	return r, err
}
