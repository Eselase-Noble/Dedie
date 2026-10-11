package oracle

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dedie/engine/connector"
)

var (
	// plsqlRe matches statements whose final semicolon is part of the
	// statement: anonymous blocks and stored program definitions.
	plsqlRe = regexp.MustCompile(`(?is)^\s*(begin|declare|create\s+(or\s+replace\s+)?((non)?editionable\s+)?(procedure|function|package|trigger|type)\b)`)
	queryRe = regexp.MustCompile(`(?is)^\s*(select|with)\b`)
	// commentRe matches leading comments, which may hide the first keyword.
	commentRe = regexp.MustCompile(`(?s)^\s*(--[^\n]*\n|/\*.*?\*/)`)
)

// Statement prepares what the user typed for the server: Oracle takes one
// statement at a time and rejects the semicolon that ends it everywhere
// except in PL/SQL.
func Statement(text string) string {
	text = strings.TrimSpace(text)
	for {
		rest := commentRe.ReplaceAllString(text, "")
		if rest == text {
			break
		}
		text = strings.TrimSpace(rest)
	}
	// A slash on its own line runs the buffer in SQL*Plus; it is not SQL.
	text = strings.TrimSpace(strings.TrimSuffix(text, "\n/"))
	if plsqlRe.MatchString(text) {
		return text
	}
	return strings.TrimRight(text, "; \t\r\n")
}

// Query runs one statement for the query browser.
//
// In read-only mode only a query is accepted, and it runs inside a READ
// ONLY transaction. Both are needed: Oracle commits before any DDL, which
// would end the transaction and then run the statement. A function called
// from a query can still write through an autonomous transaction, so the
// real boundary remains the privileges of the database account.
func (s *session) Query(ctx context.Context, text string, opts connector.QueryOptions) ([]connector.ResultSet, error) {
	if opts.MaxRows <= 0 {
		opts.MaxRows = 1000
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = 60
	}
	stmt := Statement(text)
	if stmt == "" {
		return nil, errors.New("enter a statement to run")
	}
	isQuery := queryRe.MatchString(stmt)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds)*time.Second)
	defer cancel()

	rs := connector.ResultSet{Columns: []connector.Column{}, Rows: [][]*string{}}
	if opts.ReadOnly {
		if !isQuery {
			return nil, errors.New("this connection is read-only here: only SELECT statements can be run")
		}
		if _, err := s.conn.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return nil, Friendly(err)
		}
		defer s.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	}
	if !isQuery {
		res, err := s.conn.ExecContext(ctx, stmt)
		if err != nil {
			return nil, Friendly(err)
		}
		rs.Command = "OK"
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			rs.Command = fmt.Sprintf("OK, %d row(s) affected", n)
		}
		return []connector.ResultSet{rs}, nil
	}

	rows, err := s.conn.QueryContext(ctx, stmt)
	if err != nil {
		return nil, Friendly(err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, Friendly(err)
	}
	zoned := make([]bool, len(types))
	for i, t := range types {
		name := strings.ToLower(t.DatabaseTypeName())
		rs.Columns = append(rs.Columns, connector.Column{Name: t.Name(), Type: name})
		zoned[i] = strings.Contains(name, "tz") || strings.Contains(name, "zone")
	}
	values := make([]any, len(types))
	dest := make([]any, len(types))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if len(rs.Rows) >= opts.MaxRows {
			rs.Truncated = true
			break
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, Friendly(err)
		}
		row := make([]*string, len(values))
		for i, v := range values {
			row[i] = Text(v, zoned[i])
		}
		rs.Rows = append(rs.Rows, row)
	}
	if err := rows.Err(); err != nil && !rs.Truncated {
		return nil, Friendly(err)
	}
	rs.Command = fmt.Sprintf("%d row(s)", len(rs.Rows))
	return []connector.ResultSet{rs}, nil
}

// Text renders a value the driver returned; nil stands for NULL. zoned
// keeps the time zone of a timestamp, which only matters for the types
// that store one.
func Text(v any, zoned bool) *string {
	var out string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		out = x
	case []byte:
		// Binary values are shown as hex rather than as broken text.
		if utf8.Valid(x) && len(x) > 0 && !strings.ContainsRune(string(x), 0) {
			out = string(x)
		} else {
			out = "0x" + hex.EncodeToString(x)
		}
	case time.Time:
		layout := "2006-01-02 15:04:05.999999999"
		if zoned {
			layout += " -07:00"
		}
		out = x.Format(layout)
	default:
		out = fmt.Sprint(x)
	}
	return &out
}
