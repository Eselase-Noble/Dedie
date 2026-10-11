package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector"
)

// literals are the formats a value typed in the data browser is read
// with, so a date looks the same going in as it did coming out.
var literals = []string{
	`ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS'`,
	`ALTER SESSION SET NLS_TIMESTAMP_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF'`,
	`ALTER SESSION SET NLS_TIMESTAMP_TZ_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF TZH:TZM'`,
	`ALTER SESSION SET NLS_NUMERIC_CHARACTERS = '.,'`,
}

// ChangeRow applies one edit from the data browser in a transaction, and
// rolls it back unless an update or delete touched exactly one row.
func (s *session) ChangeRow(ctx context.Context, c connector.RowChange) error {
	if err := c.Validate(); err != nil {
		return err
	}
	schema := c.Schema
	if schema == "" {
		if err := s.conn.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM dual`).Scan(&schema); err != nil {
			return Friendly(err)
		}
	}
	table := QName(schema, c.Table)
	var args []any
	// Values travel as text parameters; the server converts each to its
	// column's type using the session formats set below.
	bind := func(v *string) string {
		if v == nil {
			args = append(args, sql.NullString{})
		} else {
			args = append(args, *v)
		}
		return fmt.Sprintf(":%d", len(args))
	}
	where := func() string {
		var parts []string
		for _, col := range connector.Columns(c.Key) {
			parts = append(parts, Quote(col)+" = "+bind(c.Key[col]))
		}
		return strings.Join(parts, " AND ")
	}

	var stmt string
	switch c.Action {
	case connector.RowInsert:
		if len(c.Values) == 0 {
			return fmt.Errorf("enter a value for at least one column")
		}
		var cols, vals []string
		for _, col := range connector.Columns(c.Values) {
			cols = append(cols, Quote(col))
			vals = append(vals, bind(c.Values[col]))
		}
		stmt = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
	case connector.RowUpdate:
		var sets []string
		for _, col := range connector.Columns(c.Values) {
			sets = append(sets, Quote(col)+" = "+bind(c.Values[col]))
		}
		stmt = "UPDATE " + table + " SET " + strings.Join(sets, ", ") + " WHERE " + where()
	case connector.RowDelete:
		stmt = "DELETE FROM " + table + " WHERE " + where()
	}

	for _, set := range literals {
		if _, err := s.conn.ExecContext(ctx, set); err != nil {
			return Friendly(err)
		}
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Friendly(err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return Friendly(err)
	}
	if n, _ := res.RowsAffected(); c.Action != connector.RowInsert && n != 1 {
		return connector.ErrRowCount(n)
	}
	return Friendly(tx.Commit())
}
