package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
)

// ChangeRow applies one edit from the data browser in a transaction, and
// rolls it back unless an update or delete touched exactly one row.
func (s *session) ChangeRow(ctx context.Context, c connector.RowChange) error {
	if err := c.Validate(); err != nil {
		return err
	}
	schema := c.Schema
	if schema == "" {
		schema = "public"
	}
	table := pgx.Identifier{schema, c.Table}.Sanitize()
	var args []any
	// Values travel as text parameters; the server converts each to its
	// column's type, exactly as it would a quoted literal.
	bind := func(v *string) string {
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		return fmt.Sprintf("$%d", len(args))
	}
	where := func() string {
		var parts []string
		for _, col := range connector.Columns(c.Key) {
			parts = append(parts, pgx.Identifier{col}.Sanitize()+" = "+bind(c.Key[col]))
		}
		return strings.Join(parts, " AND ")
	}

	var stmt string
	switch c.Action {
	case connector.RowInsert:
		if len(c.Values) == 0 {
			stmt = "INSERT INTO " + table + " DEFAULT VALUES"
			break
		}
		var cols, vals []string
		for _, col := range connector.Columns(c.Values) {
			cols = append(cols, pgx.Identifier{col}.Sanitize())
			vals = append(vals, bind(c.Values[col]))
		}
		stmt = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
	case connector.RowUpdate:
		var sets []string
		for _, col := range connector.Columns(c.Values) {
			sets = append(sets, pgx.Identifier{col}.Sanitize()+" = "+bind(c.Values[col]))
		}
		stmt = "UPDATE " + table + " SET " + strings.Join(sets, ", ") + " WHERE " + where()
	case connector.RowDelete:
		stmt = "DELETE FROM " + table + " WHERE " + where()
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return friendly(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, stmt, args...)
	if err != nil {
		return friendly(err)
	}
	if c.Action != connector.RowInsert && tag.RowsAffected() != 1 {
		return connector.ErrRowCount(tag.RowsAffected())
	}
	return friendly(tx.Commit(ctx))
}
