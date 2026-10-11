package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector"
)

// Overview gathers the health snapshot shown on the administration
// screen. Sessions and parameters live in the V$ views, which an ordinary
// account cannot read; what it may not see is left out and said so.
func (s *session) Overview(ctx context.Context) (*connector.Overview, error) {
	ov := &connector.Overview{
		Databases:        []connector.DatabaseSize{},
		Activity:         []connector.Backend{},
		ReplicationSlots: []connector.ReplicationSlot{},
		Settings:         map[string]string{},
	}
	if _, version, err := Version(ctx, s.conn); err == nil {
		ov.Settings["version"] = version
	}

	// The containers this session can see: its own pluggable database, or
	// all of them from the root.
	rows, err := s.conn.QueryContext(ctx, `SELECT name, total_size FROM v$containers WHERE name <> 'PDB$SEED' ORDER BY 2 DESC`)
	if err != nil {
		ov.Settings["note"] = "this account cannot read the V$ views, so sizes, sessions and parameters are not shown; grant it SELECT_CATALOG_ROLE to see them"
		return ov, nil
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

	rows, err = s.conn.QueryContext(ctx, `SELECT s.sid, s.username, NVL(s.service_name, ' '), NVL(s.program, ' '), NVL(s.machine, ' '),
			s.status, CASE WHEN s.taddr IS NOT NULL THEN 1 ELSE 0 END, NVL(s.event, ' '), s.last_call_et,
			NVL((SELECT MAX(SUBSTR(q.sql_text, 1, 1000)) FROM v$sql q WHERE q.sql_id = NVL(s.sql_id, s.prev_sql_id)), ' ')
		FROM v$session s WHERE s.type = 'USER' AND s.username IS NOT NULL AND s.sid <> SYS_CONTEXT('USERENV','SID')
		ORDER BY DECODE(s.status, 'ACTIVE', 0, 1), s.last_call_et DESC`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var b connector.Backend
		var status string
		var inTx int
		if err := rows.Scan(&b.PID, &b.User, &b.Database, &b.Application, &b.Client, &status, &inTx, &b.WaitEvent, &b.Seconds, &b.Query); err != nil {
			rows.Close()
			return nil, err
		}
		// Map Oracle's session status onto the states the interface knows.
		switch {
		case status == "ACTIVE":
			b.State = "active"
		case inTx == 1:
			b.State = connector.StateIdleInTransaction
		default:
			b.State, b.WaitEvent = connector.StateIdle, ""
		}
		b.Database, b.Application, b.Client = strings.TrimSpace(b.Database), strings.TrimSpace(b.Application), strings.TrimSpace(b.Client)
		b.WaitEvent, b.Query = strings.TrimSpace(b.WaitEvent), strings.TrimSpace(b.Query)
		ov.Activity = append(ov.Activity, b)
	}
	rows.Close()

	rows, err = s.conn.QueryContext(ctx, `SELECT name, value FROM v$parameter WHERE name IN ('processes','sessions','undo_retention',
			'db_block_size','sga_target','pga_aggregate_target','open_cursors','enable_pluggable_database')
		UNION ALL SELECT 'log_mode', log_mode FROM v$database
		UNION ALL SELECT 'supplemental_logging', minimal FROM dba_supplemental_logging`)
	if err != nil {
		return ov, nil
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
	return ov, nil
}

// CancelBackend stops another session's statement, or the session itself
// when terminate is set. pid is the session's SID.
func (s *session) CancelBackend(ctx context.Context, pid int64, terminate bool) error {
	var serial int64
	err := s.conn.QueryRowContext(ctx, `SELECT serial# FROM v$session WHERE sid = :1`, pid).Scan(&serial)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("that session has already ended")
	}
	if err != nil {
		return Friendly(err)
	}
	stmt := fmt.Sprintf("ALTER SYSTEM CANCEL SQL '%d, %d'", pid, serial)
	if terminate {
		stmt = fmt.Sprintf("ALTER SYSTEM KILL SESSION '%d,%d' IMMEDIATE", pid, serial)
	}
	_, err = s.conn.ExecContext(ctx, stmt)
	return Friendly(err)
}

func (s *session) CreateDatabase(ctx context.Context, name string) error {
	return errors.New("a pluggable database is created by the database administrator from the container root; add a connection for its service once it exists")
}

func (s *session) CreateSchema(ctx context.Context, name string) error {
	return errors.New("an Oracle schema is a user: create the user with CREATE USER in the query editor and its schema exists with it")
}

// CreateTable creates a table in the given schema, or in the connecting
// user's own when none is named.
func (s *session) CreateTable(ctx context.Context, def connector.TableDef) error {
	if err := def.Validate(); err != nil {
		return err
	}
	var cols, pk []string
	for _, c := range def.Columns {
		col := Quote(c.Name) + " " + strings.TrimSpace(c.Type)
		if c.Default != "" {
			col += " DEFAULT " + connector.DefaultSQL(c.Default)
		}
		if !c.Nullable || c.PrimaryKey {
			col += " NOT NULL"
		}
		cols = append(cols, col)
		if c.PrimaryKey {
			pk = append(pk, Quote(c.Name))
		}
	}
	if len(pk) > 0 {
		cols = append(cols, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	name := Quote(def.Name)
	if def.Schema != "" {
		name = QName(def.Schema, def.Name)
	}
	_, err := s.conn.ExecContext(ctx, "CREATE TABLE "+name+" (\n  "+strings.Join(cols, ",\n  ")+"\n)")
	return Friendly(err)
}

// ServerObjects lists the containers this session can see. Objects in
// Oracle belong to the schema they were created in for good, so there is
// no ownership to hand over.
func (s *session) ServerObjects(ctx context.Context) (*connector.ServerObjects, error) {
	out := &connector.ServerObjects{Databases: []connector.Database{}, Roles: []string{}}
	rows, err := s.conn.QueryContext(ctx, `SELECT name FROM v$containers WHERE name <> 'PDB$SEED' ORDER BY 1`)
	if err != nil {
		var name string
		if err := s.conn.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CON_NAME') FROM dual`).Scan(&name); err != nil {
			return nil, Friendly(err)
		}
		out.Databases = append(out.Databases, connector.Database{Name: name})
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var d connector.Database
		if err := rows.Scan(&d.Name); err != nil {
			return nil, err
		}
		out.Databases = append(out.Databases, d)
	}
	return out, Friendly(rows.Err())
}

func (s *session) SetOwner(ctx context.Context, c connector.OwnerChange) error {
	return errors.New("an Oracle object belongs to the schema it was created in; to move it, recreate it in the other schema")
}

var errNoAccounts = errors.New("managing users and connection limits is available for PostgreSQL and MySQL; Oracle users and profiles are managed with its own statements in the query editor")

func (s *session) Accounts(ctx context.Context) (*connector.Accounts, error) {
	return nil, errNoAccounts
}
func (s *session) SaveAccount(ctx context.Context, c connector.AccountChange) error {
	return errNoAccounts
}
func (s *session) DropAccount(ctx context.Context, name, host string, handOver bool) error {
	return errNoAccounts
}
func (s *session) SetConnectionLimit(ctx context.Context, l connector.LimitChange) error {
	return errNoAccounts
}
func (s *session) TerminateSessions(ctx context.Context, f connector.SessionFilter) (int, error) {
	return 0, errNoAccounts
}
