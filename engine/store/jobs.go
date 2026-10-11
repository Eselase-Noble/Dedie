package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type Job struct {
	ID         string          `json:"id"`
	OrgID      string          `json:"-"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	SourceID   string          `json:"source_id"`
	TargetID   string          `json:"target_id"`
	Config     json.RawMessage `json:"config"`
	State      json.RawMessage `json:"state"`
	Preflight  json.RawMessage `json:"preflight,omitempty"`
	Phase      string          `json:"phase"`
	Status     string          `json:"status"`
	Desired    string          `json:"desired"`
	Error      string          `json:"error"`
	LeaseOwner string          `json:"-"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// Copy is filled in for a listed job that is in its initial copy, so a
	// list can show how far along it is without loading every table.
	Copy *CopyProgress `json:"copy,omitempty"`
}

const jobCols = `id,org_id,name,kind,source_id,target_id,config,state,preflight,phase,status,desired,error,lease_owner,created_by,created_at,updated_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var cfg, st, pre, created, updated string
	err := row.Scan(&j.ID, &j.OrgID, &j.Name, &j.Kind, &j.SourceID, &j.TargetID, &cfg, &st, &pre, &j.Phase, &j.Status, &j.Desired, &j.Error, &j.LeaseOwner, &j.CreatedBy, &created, &updated)
	j.Config, j.State = json.RawMessage(cfg), json.RawMessage(st)
	if pre != "" {
		j.Preflight = json.RawMessage(pre)
	}
	j.CreatedAt, j.UpdatedAt = parseTime(created), parseTime(updated)
	return j, one(err)
}

func (s *Store) CreateJob(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,org_id,name,kind,source_id,target_id,config,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.OrgID, j.Name, j.Kind, j.SourceID, j.TargetID, string(j.Config), j.CreatedBy, now(), now())
	return err
}

func (s *Store) GetJob(ctx context.Context, orgID, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE org_id=? AND id=?`, orgID, id))
}

// JobByID loads a job without an org filter; for the worker only.
func (s *Store) JobByID(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) ListJobs(ctx context.Context, orgID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE org_id=? ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) DeleteJob(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE org_id=? AND id=?`, orgID, id)
	return err
}

// SetJob updates the named columns of a job. Keys must be column names.
func (s *Store) SetJob(ctx context.Context, id string, fields map[string]any) error {
	sets, args := []string{"updated_at=?"}, []any{now()}
	for k, v := range fields {
		sets = append(sets, k+"=?")
		args = append(args, v)
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

// JobDesired returns what the operator last asked of the job.
func (s *Store) JobDesired(ctx context.Context, id string) (string, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT desired FROM jobs WHERE id=?`, id).Scan(&d)
	return d, one(err)
}

// ClaimJobs leases runnable jobs to owner: queued jobs, and running jobs whose
// previous owner stopped heartbeating (crash recovery).
func (s *Store) ClaimJobs(ctx context.Context, owner string, stale time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-stale).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `UPDATE jobs SET lease_owner=?, lease_at=?, status='running'
		WHERE status='queued' OR (status='running' AND lease_owner<>? AND lease_at<?) RETURNING id`, owner, now(), owner, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimJob leases one specific job for a foreground run.
func (s *Store) ClaimJob(ctx context.Context, id, owner string, stale time.Duration) (bool, error) {
	cutoff := time.Now().Add(-stale).UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_owner=?, lease_at=?, status='running'
		WHERE id=? AND (status='queued' OR (status='running' AND lease_at<?))`, owner, now(), id, cutoff)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Heartbeat renews a lease; it reports false when the lease was lost.
func (s *Store) Heartbeat(ctx context.Context, id, owner string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_at=? WHERE id=? AND lease_owner=?`, now(), id, owner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

type JobTable struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Status     string `json:"status"`
	EstRows    int64  `json:"est_rows"`
	RowsCopied int64  `json:"rows_copied"`
	SrcCount   int64  `json:"src_count"`
	DstCount   int64  `json:"dst_count"`
	Validation string `json:"validation"`
	Detail     string `json:"detail"`
}

func (s *Store) ResetJobTables(ctx context.Context, jobID string, tables []JobTable) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_tables WHERE job_id=?`, jobID); err != nil {
		return err
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_tables(job_id,schema_name,table_name,est_rows) VALUES(?,?,?,?)`, jobID, t.Schema, t.Table, t.EstRows); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetJobTable(ctx context.Context, jobID, schemaName, table string, fields map[string]any) error {
	var sets []string
	var args []any
	for k, v := range fields {
		sets = append(sets, k+"=?")
		args = append(args, v)
	}
	args = append(args, jobID, schemaName, table)
	_, err := s.db.ExecContext(ctx, `UPDATE job_tables SET `+strings.Join(sets, ",")+` WHERE job_id=? AND schema_name=? AND table_name=?`, args...)
	return err
}

func (s *Store) ListJobTables(ctx context.Context, jobID string) ([]JobTable, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT schema_name,table_name,status,est_rows,rows_copied,src_count,dst_count,validation,detail
		FROM job_tables WHERE job_id=? ORDER BY schema_name,table_name`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobTable{}
	for rows.Next() {
		var t JobTable
		if err := rows.Scan(&t.Schema, &t.Table, &t.Status, &t.EstRows, &t.RowsCopied, &t.SrcCount, &t.DstCount, &t.Validation, &t.Detail); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CopyProgress sums a job's initial copy over its tables.
type CopyProgress struct {
	Tables       int   `json:"tables"`
	TablesCopied int   `json:"tables_copied"`
	RowsCopied   int64 `json:"rows_copied"`
	// EstRows is the planner's estimate for all tables, as read from the
	// source when the job started. It can be zero or out of date.
	EstRows int64 `json:"est_rows"`
	// RowsExpected is the best figure for the size of the whole copy: the
	// actual count of every table already copied, and for the others the
	// estimate, or what has been copied so far where that is more.
	RowsExpected int64 `json:"rows_expected"`
}

// CopyProgress returns the progress of the initial copy for each of an
// organization's jobs in the given phase, keyed by job id.
func (s *Store) CopyProgress(ctx context.Context, orgID, phase string) (map[string]CopyProgress, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.job_id, COUNT(*), SUM(t.status='copied'), SUM(t.rows_copied), SUM(t.est_rows),
		SUM(CASE WHEN t.status='copied' OR t.rows_copied>t.est_rows THEN t.rows_copied ELSE t.est_rows END)
		FROM job_tables t JOIN jobs j ON j.id=t.job_id WHERE j.org_id=? AND j.phase=? GROUP BY t.job_id`, orgID, phase)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CopyProgress{}
	for rows.Next() {
		var id string
		var p CopyProgress
		if err := rows.Scan(&id, &p.Tables, &p.TablesCopied, &p.RowsCopied, &p.EstRows, &p.RowsExpected); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

type JobEvent struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

func (s *Store) AddJobEvent(ctx context.Context, jobID, level, msg string) {
	s.db.ExecContext(ctx, `INSERT INTO job_events(job_id,ts,level,message) VALUES(?,?,?,?)`, jobID, now(), level, msg)
}

func (s *Store) ListJobEvents(ctx context.Context, jobID string, limit int) ([]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,ts,level,message FROM job_events WHERE job_id=? ORDER BY id DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobEvent{}
	for rows.Next() {
		var e JobEvent
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Level, &e.Message); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
