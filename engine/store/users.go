package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type User struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	Disabled     bool      `json:"disabled"`
	FailedLogins int       `json:"-"`
	LockedUntil  time.Time `json:"-"`
	// TOTPSecretEnc is the sealed authenticator secret. It is set when
	// enrolment begins and counts only once TOTPEnabled is true.
	TOTPSecretEnc []byte `json:"-"`
	TOTPEnabled   bool   `json:"totp_enabled"`
	// RecoveryCodes holds hashes of the unused one-time recovery codes.
	RecoveryCodes []string  `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) CountOrgs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM orgs`).Scan(&n)
	return n, err
}

func (s *Store) CreateOrg(ctx context.Context, o Org) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO orgs(id,name,created_at) VALUES(?,?,?)`, o.ID, o.Name, now())
	return err
}

func (s *Store) GetOrg(ctx context.Context, id string) (Org, error) {
	var o Org
	err := s.db.QueryRowContext(ctx, `SELECT id,name FROM orgs WHERE id=?`, id).Scan(&o.ID, &o.Name)
	return o, one(err)
}

// FirstOrg returns the oldest organization, used by the local CLI.
func (s *Store) FirstOrg(ctx context.Context) (Org, error) {
	var o Org
	err := s.db.QueryRowContext(ctx, `SELECT id,name FROM orgs ORDER BY created_at LIMIT 1`).Scan(&o.ID, &o.Name)
	return o, one(err)
}

const userCols = `id,org_id,email,name,password_hash,role,disabled,failed_logins,locked_until,created_at,totp_secret_enc,totp_enabled,recovery_codes`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var locked, created, codes string
	err := row.Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Disabled, &u.FailedLogins, &locked, &created,
		&u.TOTPSecretEnc, &u.TOTPEnabled, &codes)
	u.LockedUntil, u.CreatedAt = parseTime(locked), parseTime(created)
	_ = json.Unmarshal([]byte(codes), &u.RecoveryCodes)
	return u, one(err)
}

func (s *Store) CreateUser(ctx context.Context, u User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,org_id,email,name,password_hash,role,created_at) VALUES(?,?,?,?,?,?,?)`,
		u.ID, u.OrgID, strings.ToLower(u.Email), u.Name, u.PasswordHash, u.Role, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("a user with that email already exists")
	}
	return err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email=?`, strings.ToLower(email)))
}

func (s *Store) UserByID(ctx context.Context, orgID, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=? AND org_id=?`, id, orgID))
}

func (s *Store) ListUsers(ctx context.Context, orgID string) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE org_id=? ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, u User) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET name=?,role=?,disabled=?,password_hash=? WHERE id=? AND org_id=?`,
		u.Name, u.Role, u.Disabled, u.PasswordHash, u.ID, u.OrgID)
	return err
}

func (s *Store) CountActiveOwners(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE org_id=? AND role='owner' AND disabled=0`, orgID).Scan(&n)
	return n, err
}

// RecordLogin resets or increments the failed-login counter; after maxFails
// consecutive failures the account is locked for lockFor.
func (s *Store) RecordLogin(ctx context.Context, userID string, ok bool, maxFails int, lockFor time.Duration) error {
	if ok {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins=0, locked_until='' WHERE id=?`, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins=failed_logins+1,
		locked_until=CASE WHEN failed_logins+1>=? THEN ? ELSE locked_until END WHERE id=?`,
		maxFails, time.Now().Add(lockFor).UTC().Format(time.RFC3339Nano), userID)
	return err
}

// SetTOTP stores a user's two-factor state: the sealed secret, whether it
// is active, and the hashes of the remaining recovery codes.
func (s *Store) SetTOTP(ctx context.Context, userID string, secretEnc []byte, enabled bool, recoveryHashes []string) error {
	codes, _ := json.Marshal(append([]string{}, recoveryHashes...))
	_, err := s.db.ExecContext(ctx, `UPDATE users SET totp_secret_enc=?, totp_enabled=?, recovery_codes=? WHERE id=?`, secretEnc, enabled, string(codes), userID)
	return err
}

// Setting returns an organization setting, or "" when it was never set.
func (s *Store) Setting(ctx context.Context, orgID, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM org_settings WHERE org_id=? AND key=?`, orgID, key).Scan(&v)
	if errors.Is(one(err), ErrNotFound) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, orgID, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_settings(org_id,key,value) VALUES(?,?,?) ON CONFLICT(org_id,key) DO UPDATE SET value=excluded.value`, orgID, key, value)
	return err
}
