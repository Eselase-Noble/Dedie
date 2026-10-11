package store

import (
	"context"
	"time"
)

// SavedQuery is a statement someone kept for later, privately or for the
// whole organization.
type SavedQuery struct {
	ID     string `json:"id"`
	OrgID  string `json:"-"`
	UserID string `json:"user_id"`
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	SQL    string `json:"sql"`
	// Kind is the database kind the statement was written for.
	Kind      string    `json:"kind"`
	Shared    bool      `json:"shared"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const queryCols = `id,org_id,user_id,owner,name,sql,kind,shared,created_at,updated_at`

func scanQuery(row interface{ Scan(...any) error }) (SavedQuery, error) {
	var q SavedQuery
	var created, updated string
	err := row.Scan(&q.ID, &q.OrgID, &q.UserID, &q.Owner, &q.Name, &q.SQL, &q.Kind, &q.Shared, &created, &updated)
	q.CreatedAt, q.UpdatedAt = parseTime(created), parseTime(updated)
	return q, one(err)
}

func (s *Store) SaveQuery(ctx context.Context, q SavedQuery) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO saved_queries(`+queryCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,sql=excluded.sql,kind=excluded.kind,shared=excluded.shared,updated_at=excluded.updated_at`,
		q.ID, q.OrgID, q.UserID, q.Owner, q.Name, q.SQL, q.Kind, q.Shared, now(), now())
	return err
}

func (s *Store) GetQuery(ctx context.Context, orgID, id string) (SavedQuery, error) {
	return scanQuery(s.db.QueryRowContext(ctx, `SELECT `+queryCols+` FROM saved_queries WHERE org_id=? AND id=?`, orgID, id))
}

// ListQueries returns a user's own saved queries and those shared with
// the organization.
func (s *Store) ListQueries(ctx context.Context, orgID, userID string) ([]SavedQuery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+queryCols+` FROM saved_queries WHERE org_id=? AND (user_id=? OR shared=1) ORDER BY name COLLATE NOCASE`, orgID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedQuery{}
	for rows.Next() {
		q, err := scanQuery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) DeleteQuery(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM saved_queries WHERE org_id=? AND id=?`, orgID, id)
	return err
}
