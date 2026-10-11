package store

import (
	"context"
	"time"
)

func (s *Store) CreateSession(ctx context.Context, tokenHash, userID, ip string, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,created_at,expires_at,ip) VALUES(?,?,?,?,?)`,
		tokenHash, userID, now(), time.Now().Add(ttl).UTC().Format(time.RFC3339Nano), ip)
	return err
}

func (s *Store) SessionUser(ctx context.Context, tokenHash string) (User, error) {
	var expires string
	row := s.db.QueryRowContext(ctx, `SELECT u.id,u.org_id,u.email,u.name,u.password_hash,u.role,u.disabled,u.failed_logins,u.locked_until,u.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, tokenHash)
	var u User
	var locked, created string
	if err := row.Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Disabled, &u.FailedLogins, &locked, &created, &expires); err != nil {
		return u, one(err)
	}
	u.CreatedAt = parseTime(created)
	if time.Now().After(parseTime(expires)) || u.Disabled {
		return u, ErrNotFound
	}
	return u, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now())
	return err
}
