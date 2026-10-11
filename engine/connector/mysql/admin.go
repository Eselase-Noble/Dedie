package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"dedie/engine/connector"
)

// Overview gathers the health snapshot shown on the administration screen.
func (s *session) Overview(ctx context.Context) (*connector.Overview, error) {
	ov := &connector.Overview{
		Databases:        []connector.DatabaseSize{},
		Activity:         []connector.Backend{},
		ReplicationSlots: []connector.ReplicationSlot{},
		Settings:         map[string]string{},
	}

	rows, err := s.conn.QueryContext(ctx, `SELECT table_schema, COALESCE(SUM(data_length + index_length), 0)
		FROM information_schema.tables WHERE table_schema `+userSchemas+` GROUP BY table_schema ORDER BY 2 DESC`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var d connector.DatabaseSize
		if err := rows.Scan(&d.Name, &d.SizeBytes); err != nil {
			rows.Close()
			return nil, err
		}
		ov.Databases = append(ov.Databases, d)
	}
	rows.Close()

	rows, err = s.conn.QueryContext(ctx, `SELECT id, user, host, COALESCE(db, ''), command, time, COALESCE(state, ''), COALESCE(LEFT(info, 2000), '')
		FROM information_schema.processlist WHERE id <> CONNECTION_ID() ORDER BY command = 'Sleep', time DESC`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var b connector.Backend
		var command string
		if err := rows.Scan(&b.PID, &b.User, &b.Client, &b.Database, &command, &b.Seconds, &b.WaitEvent, &b.Query); err != nil {
			rows.Close()
			return nil, err
		}
		// Map MySQL's command column onto the states the interface knows.
		b.State = "active"
		if command == "Sleep" {
			b.State = "idle"
		}
		b.Application = command
		ov.Activity = append(ov.Activity, b)
	}
	rows.Close()

	rows, err = s.conn.QueryContext(ctx, `SHOW GLOBAL VARIABLES WHERE Variable_name IN ('version','log_bin','binlog_format',
		'binlog_row_image','gtid_mode','server_id','max_connections','read_only','require_secure_transport',
		'innodb_buffer_pool_size','binlog_expire_logs_seconds')`)
	if err != nil {
		return nil, Friendly(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		ov.Settings[k] = v.String
	}
	return ov, Friendly(rows.Err())
}

// CancelBackend stops another connection's statement, or the connection
// itself when terminate is set.
func (s *session) CancelBackend(ctx context.Context, pid int64, terminate bool) error {
	stmt := fmt.Sprintf("KILL QUERY %d", pid)
	if terminate {
		stmt = fmt.Sprintf("KILL CONNECTION %d", pid)
	}
	_, err := s.conn.ExecContext(ctx, stmt)
	return Friendly(err)
}
