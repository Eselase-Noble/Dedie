package mymy

import (
	"context"

	"dedie/engine/migrate/viewsql"
)

// ViewColumns reads the columns of a view or table of the connection's
// database: what each is called and what kind of value it holds. It
// serves the pipelines that create translated views in MySQL, or read
// views out of it to translate them, and then compare the two; see
// viewsql.Checker. This pipeline copies view definitions as they are.
func ViewColumns(ctx context.Context, db Querier, name string) ([]viewsql.Column, error) {
	rows, err := QueryStrings(ctx, db, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position`, name)
	if err != nil {
		return nil, err
	}
	out := make([]viewsql.Column, len(rows))
	for i, r := range rows {
		out[i] = viewsql.Column{Name: r[0], Class: viewsql.MySQLClass(r[1])}
	}
	return out, nil
}
