package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Backup struct {
	ID             string `json:"id"`
	OrgID          string `json:"-"`
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name"`
	Database       string `json:"database"`
	Kind           string `json:"kind"`
	// Type is what the backup holds: full, schema, data, tables or physical.
	Type string `json:"type"`
	// Format names the archive layout, which decides how it is restored.
	Format string   `json:"format"`
	Tables []string `json:"tables"`
	// Level is full, differential or incremental. A backup that is not
	// full builds on ParentID and is useless without it.
	Level    string `json:"level"`
	ParentID string `json:"parent_id"`
	// Position is where the backup ended in the source's change log, for
	// the next backup to continue from.
	Position string `json:"position"`
	// Encrypted is false for a backup written in the database's own
	// format, readable by its tools straight from Path.
	Encrypted bool `json:"encrypted"`
	// LocationID names the storage location; empty is the data directory.
	LocationID    string    `json:"location_id"`
	ScheduleID    string    `json:"schedule_id"`
	VerifiedAt    time.Time `json:"verified_at"`
	ServerVersion string    `json:"server_version"`
	Status        string    `json:"status"`
	Error         string    `json:"error"`
	// Path is the file on the machine that runs Dedie.
	Path       string    `json:"path"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	Note       string    `json:"note"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

const backupCols = `id,org_id,connection_id,connection_name,database,kind,server_version,status,error,path,size_bytes,sha256,note,created_by,created_at,finished_at,type,format,tables,schedule_id,verified_at,level,parent_id,position,encrypted,location_id`

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var created, finished, tables, verified string
	err := row.Scan(&b.ID, &b.OrgID, &b.ConnectionID, &b.ConnectionName, &b.Database, &b.Kind, &b.ServerVersion, &b.Status, &b.Error, &b.Path, &b.SizeBytes, &b.SHA256, &b.Note, &b.CreatedBy, &created, &finished,
		&b.Type, &b.Format, &tables, &b.ScheduleID, &verified, &b.Level, &b.ParentID, &b.Position, &b.Encrypted, &b.LocationID)
	b.CreatedAt, b.FinishedAt, b.VerifiedAt = parseTime(created), parseTime(finished), parseTime(verified)
	b.Tables = []string{}
	_ = json.Unmarshal([]byte(tables), &b.Tables)
	return b, one(err)
}

func (s *Store) CreateBackup(ctx context.Context, b Backup) error {
	tables, _ := json.Marshal(append([]string{}, b.Tables...))
	_, err := s.db.ExecContext(ctx, `INSERT INTO backups(id,org_id,connection_id,connection_name,database,kind,server_version,status,path,note,created_by,created_at,type,format,tables,schedule_id,level,parent_id,encrypted,location_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.OrgID, b.ConnectionID, b.ConnectionName, b.Database, b.Kind, b.ServerVersion, b.Status, b.Path, b.Note, b.CreatedBy, now(),
		b.Type, b.Format, string(tables), b.ScheduleID, b.Level, b.ParentID, b.Encrypted, b.LocationID)
	return err
}

// SetBackupPosition records where a backup ended in the change log.
func (s *Store) SetBackupPosition(ctx context.Context, id, position string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET position=? WHERE id=?`, position, id)
	return err
}

// BackupDependents counts the backups that build directly on one.
func (s *Store) BackupDependents(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM backups WHERE parent_id=?`, id).Scan(&n)
	return n, err
}

// ConnectionBackups lists a connection's completed backups, newest first.
func (s *Store) ConnectionBackups(ctx context.Context, orgID, connID string) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE org_id=? AND connection_id=? AND status='completed' ORDER BY created_at DESC`, orgID, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Location is a directory on the machine that runs Dedie where backups
// may be written.
type Location struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"-"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Backups is how many backups are stored there.
	Backups int `json:"backups"`
}

func (s *Store) SaveLocation(ctx context.Context, l Location) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO backup_locations(id,org_id,name,path,created_by,created_at) VALUES(?,?,?,?,?,?)`,
		l.ID, l.OrgID, l.Name, l.Path, l.CreatedBy, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("a location with that name already exists")
	}
	return err
}

func (s *Store) GetLocation(ctx context.Context, orgID, id string) (Location, error) {
	var l Location
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,org_id,name,path,created_by,created_at,(SELECT count(*) FROM backups b WHERE b.location_id=l.id) FROM backup_locations l WHERE org_id=? AND (id=? OR name=?)`, orgID, id, id).
		Scan(&l.ID, &l.OrgID, &l.Name, &l.Path, &l.CreatedBy, &created, &l.Backups)
	l.CreatedAt = parseTime(created)
	return l, one(err)
}

func (s *Store) ListLocations(ctx context.Context, orgID string) ([]Location, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,org_id,name,path,created_by,created_at,(SELECT count(*) FROM backups b WHERE b.location_id=l.id) FROM backup_locations l WHERE org_id=? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Location{}
	for rows.Next() {
		var l Location
		var created string
		if err := rows.Scan(&l.ID, &l.OrgID, &l.Name, &l.Path, &l.CreatedBy, &created, &l.Backups); err != nil {
			return nil, err
		}
		l.CreatedAt = parseTime(created)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DeleteLocation(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backup_locations WHERE org_id=? AND id=?`, orgID, id)
	return err
}

// MarkBackupVerified records that the file was checked and still decrypts.
func (s *Store) MarkBackupVerified(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET verified_at=? WHERE id=?`, now(), id)
	return err
}

// ScheduleBackups lists the completed backups a schedule produced, newest first.
func (s *Store) ScheduleBackups(ctx context.Context, scheduleID string) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE schedule_id=? AND status='completed' ORDER BY created_at DESC`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
	ID         string `json:"id"`
	OrgID      string `json:"-"`
	BackupID   string `json:"backup_id"`
	TargetID   string `json:"target_id"`
	TargetName string `json:"target_name"`
	// Database is the database restored into; it differs from the
	// connection's own when the restore created a new one.
	Database   string    `json:"database"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	Log        string    `json:"log"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func (s *Store) CreateRestore(ctx context.Context, r Restore) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO restores(id,org_id,backup_id,target_id,target_name,database,status,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.OrgID, r.BackupID, r.TargetID, r.TargetName, r.Database, r.Status, r.CreatedBy, now())
	return err
}

func (s *Store) FinishRestore(ctx context.Context, id, status, errMsg, log string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE restores SET status=?,error=?,log=?,finished_at=? WHERE id=?`, status, errMsg, log, now(), id)
	return err
}

func (s *Store) ListRestores(ctx context.Context, orgID string) ([]Restore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,org_id,backup_id,target_id,target_name,database,status,error,log,created_by,created_at,finished_at FROM restores WHERE org_id=? ORDER BY created_at DESC LIMIT 100`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Restore{}
	for rows.Next() {
		var r Restore
		var created, finished string
		if err := rows.Scan(&r.ID, &r.OrgID, &r.BackupID, &r.TargetID, &r.TargetName, &r.Database, &r.Status, &r.Error, &r.Log, &r.CreatedBy, &created, &finished); err != nil {
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

// GetRestore returns one restore by id.
func (s *Store) GetRestore(ctx context.Context, orgID, id string) (Restore, error) {
	var r Restore
	var created, finished string
	err := s.db.QueryRowContext(ctx, `SELECT id,org_id,backup_id,target_id,target_name,database,status,error,log,created_by,created_at,finished_at FROM restores WHERE org_id=? AND id=?`, orgID, id).
		Scan(&r.ID, &r.OrgID, &r.BackupID, &r.TargetID, &r.TargetName, &r.Database, &r.Status, &r.Error, &r.Log, &r.CreatedBy, &created, &finished)
	r.CreatedAt, r.FinishedAt = parseTime(created), parseTime(finished)
	return r, one(err)
}
