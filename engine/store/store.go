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
`

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
