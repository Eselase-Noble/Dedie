package store

import (
	"context"
	"time"
)

// Session describes one sign-in, without the token that proves it.
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s *Store) CreateSession(ctx context.Context, id, tokenHash, userID, ip, userAgent string, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,token_hash,user_id,created_at,last_seen_at,expires_at,ip,user_agent) VALUES(?,?,?,?,?,?,?,?)`,
		id, tokenHash, userID, now(), now(), time.Now().Add(ttl).UTC().Format(time.RFC3339Nano), ip, userAgent)
	return err
}

// SessionUser resolves a session token hash to its user and the session's
// id. Expired sessions and disabled users resolve to ErrNotFound.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (User, string, error) {
	var userID, sessionID, expires, seen string
	err := s.db.QueryRowContext(ctx, `SELECT user_id,id,expires_at,last_seen_at FROM sessions WHERE token_hash=?`, tokenHash).Scan(&userID, &sessionID, &expires, &seen)
	if err != nil {
		return User{}, "", one(err)
	}
	if time.Now().After(parseTime(expires)) {
		return User{}, "", ErrNotFound
	}
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, userID))
	if err != nil || u.Disabled {
		return u, "", ErrNotFound
	}
	// Recording activity on every request would turn each read into a
	// write; once a minute is enough to show which sessions are in use.
	if time.Since(parseTime(seen)) > time.Minute {
		s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE token_hash=?`, now(), tokenHash)
	}
	return u, sessionID, nil
}

func (s *Store) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,ip,user_agent,created_at,last_seen_at,expires_at FROM sessions WHERE user_id=? AND expires_at>? ORDER BY last_seen_at DESC`, userID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var x Session
		var created, seen, expires string
		if err := rows.Scan(&x.ID, &x.UserID, &x.IP, &x.UserAgent, &created, &seen, &expires); err != nil {
			return nil, err
		}
		x.CreatedAt, x.LastSeenAt, x.ExpiresAt = parseTime(created), parseTime(seen), parseTime(expires)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// DeleteSessionByID ends one of a user's sessions.
func (s *Store) DeleteSessionByID(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id=? AND id<>''`, userID, id)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

// DeleteOtherSessions ends every session of a user except one.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keepID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id<>?`, userID, keepID)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now())
	return err
}
