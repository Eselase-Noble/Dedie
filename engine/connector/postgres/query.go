package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector"
)

// Query runs SQL for the query browser and returns every value in its text
// form, exactly as the server renders it.
//
// In read-only mode the statement goes through the extended protocol, which
// accepts only a single statement, inside a READ ONLY transaction. A user can
// therefore not end the transaction and start a writable one in the same
// request. This is a guard rail, not a substitute for connecting with a
// database role that has only the privileges the user should have.
func (s *session) Query(ctx context.Context, sql string, opts connector.QueryOptions) ([]connector.ResultSet, error) {
	pg := s.conn.PgConn()
	if opts.MaxRows <= 0 {
		opts.MaxRows = 1000
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = 60
	}
	timeout := fmt.Sprintf("SET statement_timeout = %d", opts.TimeoutSeconds*1000)

	if opts.ReadOnly {
		if _, err := pg.Exec(ctx, "BEGIN READ ONLY; "+timeout).ReadAll(); err != nil {
			return nil, friendly(err)
		}
		defer func() { pg.Exec(context.WithoutCancel(ctx), "ROLLBACK").ReadAll() }()
		rr := pg.ExecParams(ctx, sql, nil, nil, nil, nil)
		rs, err := s.readResult(ctx, rr, opts.MaxRows)
		if err != nil {
			return nil, friendly(err)
		}
		return []connector.ResultSet{rs}, nil
	}

	if _, err := pg.Exec(ctx, timeout).ReadAll(); err != nil {
		return nil, friendly(err)
	}
	mrr := pg.Exec(ctx, sql)
	var out []connector.ResultSet
	for mrr.NextResult() {
		rs, err := s.readResult(ctx, mrr.ResultReader(), opts.MaxRows)
		if err != nil {
			mrr.Close()
			return nil, friendly(err)
		}
		out = append(out, rs)
	}
	if err := mrr.Close(); err != nil {
		// A cancel sent after hitting the row limit surfaces here; the rows
		// already collected are still valid.
		if len(out) > 0 && out[len(out)-1].Truncated {
			return out, nil
		}
		return nil, friendly(err)
	}
	return out, nil
}

func (s *session) readResult(ctx context.Context, rr *pgconn.ResultReader, maxRows int) (connector.ResultSet, error) {
	rs := connector.ResultSet{Columns: []connector.Column{}, Rows: [][]*string{}}
	for _, fd := range rr.FieldDescriptions() {
		typ := fmt.Sprintf("oid:%d", fd.DataTypeOID)
		if t, ok := s.conn.TypeMap().TypeForOID(fd.DataTypeOID); ok {
			typ = t.Name
		}
		rs.Columns = append(rs.Columns, connector.Column{Name: fd.Name, Type: typ})
	}
	for rr.NextRow() {
		if len(rs.Rows) >= maxRows {
			// Stop the server from producing the rest of a large result.
			rs.Truncated = true
			_ = s.conn.PgConn().CancelRequest(ctx)
			rr.Close()
			return rs, nil
		}
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v != nil {
				str := string(v)
				row[i] = &str
			}
		}
		rs.Rows = append(rs.Rows, row)
	}
	tag, err := rr.Close()
	if err != nil {
		return rs, err
	}
	rs.Command = tag.String()
	return rs, nil
}
