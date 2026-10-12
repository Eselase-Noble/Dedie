package postgres

import (
	"context"
	"errors"
	"fmt"

	"dedie/engine/connector"
)

// Overview gathers the health snapshot shown on the administration screen:
// database sizes, live sessions, replication slots and key settings.
func (s *session) Overview(ctx context.Context) (*connector.Overview, error) {
	ov := &connector.Overview{
		Databases:        []connector.DatabaseSize{},
		Activity:         []connector.Backend{},
		ReplicationSlots: []connector.ReplicationSlot{},
		Settings:         map[string]string{},
	}

	rows, err := s.conn.Query(ctx, `SELECT datname,
			CASE WHEN has_database_privilege(datname,'CONNECT') THEN pg_database_size(datname) ELSE -1 END
		FROM pg_database WHERE NOT datistemplate ORDER BY 2 DESC`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var d connector.DatabaseSize
		if err := rows.Scan(&d.Name, &d.SizeBytes); err != nil {
			return nil, err
		}
		ov.Databases = append(ov.Databases, d)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT pid, coalesce(usename,''), coalesce(datname,''), coalesce(application_name,''),
			coalesce(client_addr::text,'local'), coalesce(state,''), coalesce(wait_event_type || ': ' || wait_event,''),
			coalesce(extract(epoch FROM now() - coalesce(query_start, backend_start))::bigint, 0), coalesce(left(query, 2000),'')
		FROM pg_stat_activity WHERE backend_type IN ('client backend','walsender') AND pid <> pg_backend_pid()
		ORDER BY (state = 'active') DESC, query_start`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var b connector.Backend
		if err := rows.Scan(&b.PID, &b.User, &b.Database, &b.Application, &b.Client, &b.State, &b.WaitEvent, &b.Seconds, &b.Query); err != nil {
			return nil, err
		}
		ov.Activity = append(ov.Activity, b)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	// Retained WAL per slot is the number that fills disks when a consumer
	// stops reading, so it is surfaced prominently in the UI.
	rows, err = s.conn.Query(ctx, `SELECT slot_name, coalesce(plugin,''), coalesce(database,''), active,
			coalesce(pg_wal_lsn_diff(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END, restart_lsn), 0)::bigint
		FROM pg_replication_slots ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var r connector.ReplicationSlot
		if err := rows.Scan(&r.Name, &r.Plugin, &r.Database, &r.Active, &r.RetainedBytes); err != nil {
			return nil, err
		}
		ov.ReplicationSlots = append(ov.ReplicationSlots, r)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT name, setting || coalesce(' ' || unit, '') FROM pg_settings
		WHERE name IN ('server_version','wal_level','max_connections','max_replication_slots','max_wal_senders',
			'shared_buffers','ssl','password_encryption','archive_mode','log_connections') ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		ov.Settings[k] = v
	}
	return ov, friendly(rows.Err())
}

func (s *session) CancelBackend(ctx context.Context, pid int64, terminate bool) error {
	fn := "pg_cancel_backend"
	if terminate {
		fn = "pg_terminate_backend"
	}
	var ok bool
	if err := s.conn.QueryRow(ctx, "SELECT "+fn+"($1)", pid).Scan(&ok); err != nil {
		return friendly(err)
	}
	if !ok {
		return errors.New("backend not found or already gone")
	}
	if terminate && s.awaitExit(ctx, []int64{pid}) > 0 {
		return fmt.Errorf("session %d was told to end and has not gone yet; it is probably rolling back its transaction. Refresh in a moment", pid)
	}
	return nil
}
