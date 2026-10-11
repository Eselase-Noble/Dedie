package mysql

import (
	"context"
	"strings"

	"dedie/engine/connector"
)

// ChangeRow applies one edit from the data browser in a transaction, and
// rolls it back unless an update or delete touched exactly one row.
func (s *session) ChangeRow(ctx context.Context, c connector.RowChange) error {
	if err := c.Validate(); err != nil {
		return err
	}
	table := Quote(c.Table)
	if c.Schema != "" {
		table = Quote(c.Schema) + "." + table
	}
	var args []any
	bind := func(v *string) string {
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		return "?"
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

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Friendly(err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return Friendly(err)
	}
	// The connection counts matched rows, so an update that sets a value
	// to what it already is still reports one.
	if n, _ := res.RowsAffected(); c.Action != connector.RowInsert && n != 1 {
		return connector.ErrRowCount(n)
	}
	return Friendly(tx.Commit())
}
