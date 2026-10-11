package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Schedule is a recurring backup of one connection.
type Schedule struct {
	ID           string   `json:"id"`
	OrgID        string   `json:"-"`
	Name         string   `json:"name"`
	ConnectionID string   `json:"connection_id"`
	Type         string   `json:"type"`
	Tables       []string `json:"tables"`
	// Cron is a five-field expression evaluated in the server's time zone.
	Cron string `json:"cron"`
	// KeepLast and KeepDays bound how many of the schedule's backups are
	// kept; zero disables the rule.
	KeepLast   int       `json:"keep_last"`
	KeepDays   int       `json:"keep_days"`
	Verify     bool      `json:"verify"`
	Enabled    bool      `json:"enabled"`
	LastRunAt  time.Time `json:"last_run_at"`
	LastStatus string    `json:"last_status"`
	LastError  string    `json:"last_error"`
	NextRunAt  time.Time `json:"next_run_at"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

const scheduleCols = `id,org_id,name,connection_id,type,tables,cron,keep_last,keep_days,verify,enabled,last_run_at,last_status,last_error,next_run_at,created_by,created_at`

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	var tables, last, next, created string
	err := row.Scan(&sc.ID, &sc.OrgID, &sc.Name, &sc.ConnectionID, &sc.Type, &tables, &sc.Cron, &sc.KeepLast, &sc.KeepDays,
		&sc.Verify, &sc.Enabled, &last, &sc.LastStatus, &sc.LastError, &next, &sc.CreatedBy, &created)
	sc.LastRunAt, sc.NextRunAt, sc.CreatedAt = parseTime(last), parseTime(next), parseTime(created)
	sc.Tables = []string{}
	_ = json.Unmarshal([]byte(tables), &sc.Tables)
	return sc, one(err)
}

// SaveSchedule creates a schedule or replaces its definition. Run history
// is kept on update.
func (s *Store) SaveSchedule(ctx context.Context, sc Schedule) error {
	tables, _ := json.Marshal(append([]string{}, sc.Tables...))
	_, err := s.db.ExecContext(ctx, `INSERT INTO backup_schedules(id,org_id,name,connection_id,type,tables,cron,keep_last,keep_days,verify,enabled,next_run_at,created_by,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,connection_id=excluded.connection_id,type=excluded.type,tables=excluded.tables,
		cron=excluded.cron,keep_last=excluded.keep_last,keep_days=excluded.keep_days,verify=excluded.verify,enabled=excluded.enabled,next_run_at=excluded.next_run_at`,
		sc.ID, sc.OrgID, sc.Name, sc.ConnectionID, sc.Type, string(tables), sc.Cron, sc.KeepLast, sc.KeepDays, sc.Verify, sc.Enabled, stamp(sc.NextRunAt), sc.CreatedBy, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("a schedule with that name already exists")
	}
	return err
}

func (s *Store) GetSchedule(ctx context.Context, orgID, id string) (Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM backup_schedules WHERE org_id=? AND id=?`, orgID, id))
}

func (s *Store) ListSchedules(ctx context.Context, orgID string) ([]Schedule, error) {
	return s.schedules(ctx, `SELECT `+scheduleCols+` FROM backup_schedules WHERE org_id=? ORDER BY name`, orgID)
}

// DueSchedules lists enabled schedules, in every organization, whose next
// run time has passed.
func (s *Store) DueSchedules(ctx context.Context, at time.Time) ([]Schedule, error) {
	return s.schedules(ctx, `SELECT `+scheduleCols+` FROM backup_schedules WHERE enabled=1 AND next_run_at<>'' AND next_run_at<=?`, stamp(at))
}

func (s *Store) schedules(ctx context.Context, query string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ClaimSchedule moves a due schedule to its next run time. It reports false
// when another process got there first, so a run is never started twice.
func (s *Store) ClaimSchedule(ctx context.Context, id string, due, next time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE backup_schedules SET next_run_at=?, last_run_at=?, last_status='running', last_error='' WHERE id=? AND next_run_at=?`,
		stamp(next), now(), id, stamp(due))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) FinishSchedule(ctx context.Context, id, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backup_schedules SET last_run_at=?, last_status=?, last_error=? WHERE id=?`, now(), status, errMsg, id)
	return err
}

func (s *Store) DeleteSchedule(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backup_schedules WHERE org_id=? AND id=?`, orgID, id)
	return err
}
