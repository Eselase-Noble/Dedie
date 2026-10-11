package store

import (
	"context"
	"time"
)

// WALArchive is the continuous archive of a PostgreSQL server's
// write-ahead log, which is what makes recovery to a moment possible.
type WALArchive struct {
	ConnectionID string `json:"connection_id"`
	OrgID        string `json:"-"`
	Enabled      bool   `json:"enabled"`
	// Dir is where the log segments are kept, on the machine that runs Dedie.
	Dir string `json:"dir"`
	// Slot is the replication slot that keeps the server from discarding
	// log the archive has not received yet.
	Slot string `json:"slot"`
	// RetentionDays deletes segments older than this; zero keeps them all.
	RetentionDays int       `json:"retention_days"`
	StartedAt     time.Time `json:"started_at"`
	LastError     string    `json:"last_error"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const walCols = `connection_id,org_id,enabled,dir,slot,retention_days,started_at,last_error,updated_by,updated_at`

func scanWAL(row interface{ Scan(...any) error }) (WALArchive, error) {
	var w WALArchive
	var started, updated string
	err := row.Scan(&w.ConnectionID, &w.OrgID, &w.Enabled, &w.Dir, &w.Slot, &w.RetentionDays, &started, &w.LastError, &w.UpdatedBy, &updated)
	w.StartedAt, w.UpdatedAt = parseTime(started), parseTime(updated)
	return w, one(err)
}

func (s *Store) GetWALArchive(ctx context.Context, connID string) (WALArchive, error) {
	return scanWAL(s.db.QueryRowContext(ctx, `SELECT `+walCols+` FROM wal_archives WHERE connection_id=?`, connID))
}

func (s *Store) SaveWALArchive(ctx context.Context, w WALArchive) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wal_archives(connection_id,org_id,enabled,dir,slot,retention_days,started_at,updated_by,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(connection_id) DO UPDATE SET enabled=excluded.enabled,retention_days=excluded.retention_days,started_at=excluded.started_at,
		last_error='',updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
		w.ConnectionID, w.OrgID, w.Enabled, w.Dir, w.Slot, w.RetentionDays, stamp(w.StartedAt), w.UpdatedBy, now())
	return err
}

// WALArchives lists the archives of one organization, or of all when
// orgID is empty.
func (s *Store) WALArchives(ctx context.Context, orgID string) ([]WALArchive, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+walCols+` FROM wal_archives WHERE ?='' OR org_id=?`, orgID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WALArchive{}
	for rows.Next() {
		w, err := scanWAL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) SetWALError(ctx context.Context, connID, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE wal_archives SET last_error=? WHERE connection_id=? AND last_error<>?`, msg, connID, msg)
	return err
}
