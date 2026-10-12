package pgpg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"dedie/engine/migrate/viewsql"
)

// The functions here serve the pipelines that create translated views in
// PostgreSQL, or read views out of it to translate them, and then compare
// the two; see viewsql.Checker. This pipeline copies view definitions as
// they are and has no use for them itself.

// TextRow runs a query that returns one row of text values and gives them
// back, nil for NULL.
func TextRow(ctx context.Context, conn *pgx.Conn, query string) ([]*string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, pgx.ErrNoRows
	}
	values, err := rows.Values()
	if err != nil {
		return nil, err
	}
	out := make([]*string, len(values))
	for i, v := range values {
		if s, ok := v.(string); ok {
			out[i] = &s
		}
	}
	rows.Close()
	return out, rows.Err()
}

// ViewColumns reads the columns of a view or table, given its quoted,
// schema-qualified name: what each is called and what kind of value it
// holds.
func ViewColumns(ctx context.Context, conn *pgx.Conn, quoted string) ([]viewsql.Column, error) {
	rows, err := conn.Query(ctx, `SELECT a.attname, t.typname, t.typcategory::text
		FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, quoted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []viewsql.Column
	for rows.Next() {
		var name, typ, category string
		if err := rows.Scan(&name, &typ, &category); err != nil {
			return nil, err
		}
		out = append(out, viewsql.Column{Name: name, Class: viewsql.PostgresClass(typ, category)})
	}
	return out, rows.Err()
}
