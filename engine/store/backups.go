package store

import (
	"context"
	"time"
)

type Backup struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"-"`
	ConnectionID   string    `json:"connection_id"`
	ConnectionName string    `json:"connection_name"`
	Database       string    `json:"database"`
	Kind           string    `json:"kind"`
	ServerVersion  string    `json:"server_version"`
	Status         string    `json:"status"`
	Error          string    `json:"error"`
	Path           string    `json:"-"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	Note           string    `json:"note"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

const backupCols = `id,org_id,connection_id,connection_name,database,kind,server_version,status,error,path,size_bytes,sha256,note,created_by,created_at,finished_at`

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var created, finished string
	err := row.Scan(&b.ID, &b.OrgID, &b.ConnectionID, &b.ConnectionName, &b.Database, &b.Kind, &b.ServerVersion, &b.Status, &b.Error, &b.Path, &b.SizeBytes, &b.SHA256, &b.Note, &b.CreatedBy, &created, &finished)
	b.CreatedAt, b.FinishedAt = parseTime(created), parseTime(finished)
	return b, one(err)
}

func (s *Store) CreateBackup(ctx context.Context, b Backup) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO backups(id,org_id,connection_id,connection_name,database,kind,server_version,status,path,note,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.OrgID, b.ConnectionID, b.ConnectionName, b.Database, b.Kind, b.ServerVersion, b.Status, b.Path, b.Note, b.CreatedBy, now())
	return err
}

func (s *Store) FinishBackup(ctx context.Context, id, status, errMsg string, size int64, sum string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET status=?,error=?,size_bytes=?,sha256=?,finished_at=? WHERE id=?`, status, errMsg, size, sum, now(), id)
	return err
}

func (s *Store) GetBackup(ctx context.Context, orgID, id string) (Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE org_id=? AND id=?`, orgID, id))
}

func (s *Store) ListBackups(ctx context.Context, orgID string) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE org_id=? ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBackup(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE org_id=? AND id=?`, orgID, id)
	return err
}

type Restore struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"-"`
	BackupID   string    `json:"backup_id"`
	TargetID   string    `json:"target_id"`
	TargetName string    `json:"target_name"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	Log        string    `json:"log"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func (s *Store) CreateRestore(ctx context.Context, r Restore) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO restores(id,org_id,backup_id,target_id,target_name,status,created_by,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.OrgID, r.BackupID, r.TargetID, r.TargetName, r.Status, r.CreatedBy, now())
	return err
}

func (s *Store) FinishRestore(ctx context.Context, id, status, errMsg, log string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE restores SET status=?,error=?,log=?,finished_at=? WHERE id=?`, status, errMsg, log, now(), id)
	return err
}

func (s *Store) ListRestores(ctx context.Context, orgID string) ([]Restore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,org_id,backup_id,target_id,target_name,status,error,log,created_by,created_at,finished_at FROM restores WHERE org_id=? ORDER BY created_at DESC LIMIT 100`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Restore{}
	for rows.Next() {
		var r Restore
		var created, finished string
		if err := rows.Scan(&r.ID, &r.OrgID, &r.BackupID, &r.TargetID, &r.TargetName, &r.Status, &r.Error, &r.Log, &r.CreatedBy, &created, &finished); err != nil {
			return nil, err
		}
		r.CreatedAt, r.FinishedAt = parseTime(created), parseTime(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// FailInterrupted marks backups and restores left "running" by a previous
// process as failed; they cannot be resumed.
func (s *Store) FailInterrupted(ctx context.Context) {
	s.db.ExecContext(ctx, `UPDATE backups SET status='failed', error='interrupted by restart', finished_at=? WHERE status='running'`, now())
	s.db.ExecContext(ctx, `UPDATE restores SET status='failed', error='interrupted by restart', finished_at=? WHERE status='running'`, now())
}
