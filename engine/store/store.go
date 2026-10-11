// Package store is the engine's metadata database (SQLite): organizations,
// users, sessions, saved connections, migration jobs, backups and the audit log.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS orgs (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  email TEXT NOT NULL UNIQUE, name TEXT NOT NULL, password_hash TEXT NOT NULL,
  role TEXT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0,
  failed_logins INTEGER NOT NULL DEFAULT 0, locked_until TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, ip TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS connections (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  name TEXT NOT NULL, kind TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL,
  database TEXT NOT NULL, username TEXT NOT NULL, secret_enc BLOB NOT NULL,
  sslmode TEXT NOT NULL, ssl_root_cert TEXT NOT NULL DEFAULT '',
  read_only INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  UNIQUE(org_id, name));
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  name TEXT NOT NULL, kind TEXT NOT NULL, source_id TEXT NOT NULL, target_id TEXT NOT NULL,
  config TEXT NOT NULL DEFAULT '{}', state TEXT NOT NULL DEFAULT '{}', preflight TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'draft', desired TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', lease_owner TEXT NOT NULL DEFAULT '', lease_at TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS job_tables (
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  schema_name TEXT NOT NULL, table_name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
  est_rows INTEGER NOT NULL DEFAULT 0, rows_copied INTEGER NOT NULL DEFAULT 0,
  src_count INTEGER NOT NULL DEFAULT -1, dst_count INTEGER NOT NULL DEFAULT -1,
  validation TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, schema_name, table_name));
CREATE TABLE IF NOT EXISTS job_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  ts TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS job_events_job ON job_events(job_id, id);
CREATE TABLE IF NOT EXISTS backups (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  connection_id TEXT NOT NULL, connection_name TEXT NOT NULL, database TEXT NOT NULL,
  kind TEXT NOT NULL, server_version TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', size_bytes INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL, created_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS restores (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  backup_id TEXT NOT NULL, target_id TEXT NOT NULL, target_name TEXT NOT NULL,
  status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', log TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL, created_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, org_id TEXT NOT NULL, ts TEXT NOT NULL,
  actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
  prev_hash TEXT NOT NULL, hash TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS audit_org ON audit(org_id, id);
CREATE TABLE IF NOT EXISTS backup_schedules (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  name TEXT NOT NULL, connection_id TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'full',
  tables TEXT NOT NULL DEFAULT '[]', cron TEXT NOT NULL,
  keep_last INTEGER NOT NULL DEFAULT 0, keep_days INTEGER NOT NULL DEFAULT 0,
  verify INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT NOT NULL DEFAULT '', last_status TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
  next_run_at TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  UNIQUE(org_id, name));
CREATE TABLE IF NOT EXISTS api_tokens (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
  expires_at TEXT NOT NULL DEFAULT '', last_used_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS org_settings (
  org_id TEXT NOT NULL REFERENCES orgs(id), key TEXT NOT NULL, value TEXT NOT NULL,
  PRIMARY KEY (org_id, key));
CREATE TABLE IF NOT EXISTS connection_grants (
  connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (connection_id, user_id));
CREATE TABLE IF NOT EXISTS idle_policies (
  connection_id TEXT PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE, org_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0, idle_minutes INTEGER NOT NULL DEFAULT 30, tx_minutes INTEGER NOT NULL DEFAULT 0,
  min_usage_percent INTEGER NOT NULL DEFAULT 0, exclude_users TEXT NOT NULL DEFAULT '[]',
  last_run_at TEXT NOT NULL DEFAULT '', last_closed INTEGER NOT NULL DEFAULT 0, total_closed INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS wal_archives (
  connection_id TEXT PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE, org_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0, dir TEXT NOT NULL, slot TEXT NOT NULL, retention_days INTEGER NOT NULL DEFAULT 0,
  started_at TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS backup_locations (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  name TEXT NOT NULL, path TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  UNIQUE(org_id, name));
CREATE TABLE IF NOT EXISTS saved_queries (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  user_id TEXT NOT NULL, owner TEXT NOT NULL, name TEXT NOT NULL, sql TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT '', shared INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS alert_channels (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id),
  name TEXT NOT NULL, kind TEXT NOT NULL, config_enc BLOB NOT NULL, summary TEXT NOT NULL DEFAULT '',
  events TEXT NOT NULL DEFAULT '[]', enabled INTEGER NOT NULL DEFAULT 1,
  last_sent_at TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(org_id, name));
CREATE TABLE IF NOT EXISTS alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, org_id TEXT NOT NULL, ts TEXT NOT NULL,
  type TEXT NOT NULL, subject TEXT NOT NULL, message TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS alert_events_org ON alert_events(org_id, id);
CREATE TABLE IF NOT EXISTS health_samples (
  connection_id TEXT NOT NULL, ts TEXT NOT NULL, online INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS health_samples_conn ON health_samples(connection_id, ts);
CREATE TABLE IF NOT EXISTS notifications (
  id INTEGER PRIMARY KEY AUTOINCREMENT, org_id TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '',
  topic TEXT NOT NULL, kind TEXT NOT NULL, level TEXT NOT NULL, title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '', link TEXT NOT NULL DEFAULT '',
  connection_id TEXT NOT NULL DEFAULT '', target_id TEXT NOT NULL DEFAULT '', job_id TEXT NOT NULL DEFAULT '',
  once_key TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS notifications_org ON notifications(org_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS notifications_once ON notifications(org_id, once_key) WHERE once_key <> '';
CREATE TABLE IF NOT EXISTS notification_reads (
  user_id TEXT NOT NULL, notification_id INTEGER NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, notification_id)) WITHOUT ROWID;
`

// columns lists columns added to existing tables after the first release.
// SQLite has no ADD COLUMN IF NOT EXISTS, so each is added only when missing.
var columns = []struct{ table, name, def string }{
	{"backups", "type", "TEXT NOT NULL DEFAULT 'full'"},
	{"backups", "format", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "tables", "TEXT NOT NULL DEFAULT '[]'"},
	{"backups", "schedule_id", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "verified_at", "TEXT NOT NULL DEFAULT ''"},
	{"restores", "database", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "level", "TEXT NOT NULL DEFAULT 'full'"},
	{"backups", "parent_id", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "position", "TEXT NOT NULL DEFAULT ''"},
	{"backups", "encrypted", "INTEGER NOT NULL DEFAULT 1"},
	{"backups", "location_id", "TEXT NOT NULL DEFAULT ''"},
	{"backup_locations", "kind", "TEXT NOT NULL DEFAULT 'dir'"},
	{"backup_locations", "config_enc", "BLOB"},
	{"backup_schedules", "level", "TEXT NOT NULL DEFAULT 'full'"},
	{"backup_schedules", "location_id", "TEXT NOT NULL DEFAULT ''"},
	{"backup_schedules", "encrypted", "INTEGER NOT NULL DEFAULT 1"},
	{"users", "totp_secret_enc", "BLOB"},
	{"users", "totp_enabled", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "recovery_codes", "TEXT NOT NULL DEFAULT '[]'"},
	{"sessions", "id", "TEXT NOT NULL DEFAULT ''"},
	{"sessions", "user_agent", "TEXT NOT NULL DEFAULT ''"},
	{"sessions", "last_seen_at", "TEXT NOT NULL DEFAULT ''"},
	{"connections", "restricted", "INTEGER NOT NULL DEFAULT 0"},
}

// upgrade brings a database created by an older release up to date.
func upgrade(db *sql.DB) error {
	for _, c := range columns {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return err
		}
	}
	return nil
}

// Open opens (and creates if needed) the metadata database in dataDir.
func Open(dataDir string) (*Store, error) {
	// Create the file ourselves so it is private from the start; SQLite gives
	// its journal files the same permissions as the database.
	path := filepath.Join(dataDir, "dedie.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init metadata db: %w", err)
	}
	if err := upgrade(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade metadata db: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func one(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
