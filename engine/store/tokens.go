package store

import (
	"context"
	"time"
)

// APIToken is a long-lived credential for scripts and the remote CLI. Only
// a hash of the token itself is stored.
type APIToken struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"-"`
	UserID     string    `json:"user_id"`
	UserEmail  string    `json:"user_email"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Store) CreateAPIToken(ctx context.Context, t APIToken, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(id,org_id,user_id,name,token_hash,role,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.OrgID, t.UserID, t.Name, tokenHash, t.Role, stamp(t.ExpiresAt), now())
	return err
}

const tokenCols = `t.id,t.org_id,t.user_id,u.email,t.name,t.role,t.expires_at,t.last_used_at,t.created_at`

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var expires, used, created string
	err := row.Scan(&t.ID, &t.OrgID, &t.UserID, &t.UserEmail, &t.Name, &t.Role, &expires, &used, &created)
	t.ExpiresAt, t.LastUsedAt, t.CreatedAt = parseTime(expires), parseTime(used), parseTime(created)
	return t, one(err)
}

// TokenByHash resolves a presented token. Expired tokens are not found.
func (s *Store) TokenByHash(ctx context.Context, tokenHash string) (APIToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=?`, tokenHash))
	if err != nil {
		return t, err
	}
	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return t, ErrNotFound
	}
	if time.Since(t.LastUsedAt) > time.Minute {
		s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, now(), t.ID)
	}
	return t, nil
}

// ListAPITokens lists an organization's tokens, or one user's when userID
// is set.
func (s *Store) ListAPITokens(ctx context.Context, orgID, userID string) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.org_id=? AND (?='' OR t.user_id=?) ORDER BY t.created_at DESC`, orgID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetAPIToken(ctx context.Context, orgID, id string) (APIToken, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.org_id=? AND t.id=?`, orgID, id))
}

func (s *Store) DeleteAPIToken(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE org_id=? AND id=?`, orgID, id)
	return err
}

func (s *Store) DeleteUserTokens(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id=?`, userID)
	return err
}
