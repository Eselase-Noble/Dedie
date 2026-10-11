package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"dedie/engine/connector"
)

// Query runs one statement for the query browser.
//
// In read-only mode the session itself is switched to read-only before the
// statement runs, so writes and schema changes are refused by the server.
// The driver sends a single statement per request, which means a user cannot
// lift the setting and write in the same call. As with every such guard, the
// real boundary is the privileges of the database account.
func (s *session) Query(ctx context.Context, query string, opts connector.QueryOptions) ([]connector.ResultSet, error) {
	if opts.MaxRows <= 0 {
		opts.MaxRows = 1000
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = 60
	}
	// max_execution_time applies to SELECT statements only.
	if _, err := s.conn.ExecContext(ctx, fmt.Sprintf("SET SESSION max_execution_time = %d", opts.TimeoutSeconds*1000)); err != nil {
		return nil, Friendly(err)
	}
	if opts.ReadOnly {
		if _, err := s.conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
			return nil, Friendly(err)
		}
	}

	// Cancelling this context is how the rest of an oversized result is
	// abandoned: the driver drops the connection instead of reading it all.
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, err := s.conn.QueryContext(qctx, query)
	if err != nil {
		return nil, Friendly(err)
	}
	defer rows.Close()

	rs := connector.ResultSet{Columns: []connector.Column{}, Rows: [][]*string{}}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, Friendly(err)
	}
	for _, t := range types {
		rs.Columns = append(rs.Columns, connector.Column{Name: t.Name(), Type: strings.ToLower(t.DatabaseTypeName())})
	}
	if len(types) == 0 {
		rows.Close()
		var affected int64
		_ = s.conn.QueryRowContext(ctx, "SELECT ROW_COUNT()").Scan(&affected)
		rs.Command = "OK"
		if affected >= 0 {
			rs.Command = fmt.Sprintf("OK, %d row(s) affected", affected)
		}
		return []connector.ResultSet{rs}, nil
	}

	raw := make([]sql.RawBytes, len(types))
	dest := make([]any, len(types))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if len(rs.Rows) >= opts.MaxRows {
			rs.Truncated = true
			cancel()
			return []connector.ResultSet{rs}, nil
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, Friendly(err)
		}
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v == nil {
				continue
			}
			// Binary values are shown as hex rather than as broken text.
			text := string(v)
			if !utf8.Valid(v) {
				text = "0x" + hex.EncodeToString(v)
			}
			row[i] = &text
		}
		rs.Rows = append(rs.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, Friendly(err)
	}
	rs.Command = fmt.Sprintf("%d row(s)", len(rs.Rows))
	return []connector.ResultSet{rs}, nil
}
