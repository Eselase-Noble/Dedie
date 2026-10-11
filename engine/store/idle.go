package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// IdlePolicy is a server's rule for closing idle connections automatically.
type IdlePolicy struct {
	ConnectionID string `json:"connection_id"`
	OrgID        string `json:"-"`
	Enabled      bool   `json:"enabled"`
	// IdleMinutes is how long a connection may sit idle before it is closed.
	IdleMinutes int `json:"idle_minutes"`
	// TxMinutes is the same for a connection idle inside an open
	// transaction; zero leaves those alone.
	TxMinutes int `json:"tx_minutes"`
	// MinUsagePercent makes the rule act only while the server is at least
	// this full; zero means always.
	MinUsagePercent int `json:"min_usage_percent"`
	// ExcludeUsers lists database users whose connections are never closed.
	ExcludeUsers []string  `json:"exclude_users"`
	LastRunAt    time.Time `json:"last_run_at"`
	LastClosed   int       `json:"last_closed"`
	TotalClosed  int       `json:"total_closed"`
	LastError    string    `json:"last_error"`
	UpdatedBy    string    `json:"updated_by"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const idleCols = `connection_id,org_id,enabled,idle_minutes,tx_minutes,min_usage_percent,exclude_users,last_run_at,last_closed,total_closed,last_error,updated_by,updated_at`

func scanIdle(row interface{ Scan(...any) error }) (IdlePolicy, error) {
	var p IdlePolicy
	var users, run, updated string
	err := row.Scan(&p.ConnectionID, &p.OrgID, &p.Enabled, &p.IdleMinutes, &p.TxMinutes, &p.MinUsagePercent, &users, &run, &p.LastClosed, &p.TotalClosed, &p.LastError, &p.UpdatedBy, &updated)
	p.LastRunAt, p.UpdatedAt = parseTime(run), parseTime(updated)
	p.ExcludeUsers = []string{}
	_ = json.Unmarshal([]byte(users), &p.ExcludeUsers)
	return p, one(err)
}

// GetIdlePolicy returns a connection's rule, or the default, switched
// off, when none was saved.
func (s *Store) GetIdlePolicy(ctx context.Context, orgID, connID string) (IdlePolicy, error) {
	p, err := scanIdle(s.db.QueryRowContext(ctx, `SELECT `+idleCols+` FROM idle_policies WHERE org_id=? AND connection_id=?`, orgID, connID))
	if errors.Is(err, ErrNotFound) {
		return IdlePolicy{ConnectionID: connID, OrgID: orgID, IdleMinutes: 30, ExcludeUsers: []string{}}, nil
	}
	return p, err
}

func (s *Store) SaveIdlePolicy(ctx context.Context, p IdlePolicy) error {
	users, _ := json.Marshal(append([]string{}, p.ExcludeUsers...))
	_, err := s.db.ExecContext(ctx, `INSERT INTO idle_policies(connection_id,org_id,enabled,idle_minutes,tx_minutes,min_usage_percent,exclude_users,updated_by,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(connection_id) DO UPDATE SET enabled=excluded.enabled,idle_minutes=excluded.idle_minutes,tx_minutes=excluded.tx_minutes,
		min_usage_percent=excluded.min_usage_percent,exclude_users=excluded.exclude_users,updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
		p.ConnectionID, p.OrgID, p.Enabled, p.IdleMinutes, p.TxMinutes, p.MinUsagePercent, string(users), p.UpdatedBy, now())
	return err
}

// EnabledIdlePolicies lists the rules that are switched on, in every organization.
func (s *Store) EnabledIdlePolicies(ctx context.Context) ([]IdlePolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+idleCols+` FROM idle_policies WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdlePolicy
	for rows.Next() {
		p, err := scanIdle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordIdleRun notes the outcome of one pass of a rule.
func (s *Store) RecordIdleRun(ctx context.Context, connID string, closed int, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE idle_policies SET last_run_at=?, last_closed=?, total_closed=total_closed+?, last_error=? WHERE connection_id=?`,
		now(), closed, closed, errMsg, connID)
	return err
}
