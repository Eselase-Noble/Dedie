package app

import (
	"context"
	"time"

	"dedie/engine/secure"
	"dedie/engine/store"
)

func totpAAD(userID string) []byte { return []byte("totp/" + userID) }

// self reloads the acting user, so that account changes are checked
// against what is stored now rather than when the session was resolved.
func (a *App) self(ctx context.Context, actor Actor) (store.User, error) {
	if err := actor.interactive(); err != nil {
		return store.User{}, err
	}
	u, err := a.Store.UserByID(ctx, actor.User.OrgID, actor.User.ID)
	return u, wrap(err)
}

// ChangePassword sets a new password for the signed-in user and ends their
// other sessions.
func (a *App) ChangePassword(ctx context.Context, actor Actor, current, next string) error {
	u, err := a.self(ctx, actor)
	if err != nil {
		return err
	}
	if !secure.VerifyPassword(u.PasswordHash, current) {
		return &Error{Status: 403, Message: "the current password is not correct"}
	}
	if err := a.policy(ctx, u.OrgID).checkPassword(next); err != nil {
		return err
	}
	if u.PasswordHash, err = secure.HashPassword(next); err != nil {
		return err
	}
	if err := a.Store.UpdateUser(ctx, u); err != nil {
		return err
	}
	a.Store.DeleteOtherSessions(ctx, u.ID, actor.SessionID)
	a.audit(ctx, actor, "auth.password_change", u.ID, nil)
	return nil
}

// SessionInfo is a session as its owner sees it.
type SessionInfo struct {
	store.Session
	Current bool `json:"current"`
}

func (a *App) ListSessions(ctx context.Context, actor Actor) ([]SessionInfo, error) {
	list, err := a.Store.ListSessions(ctx, actor.User.ID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, len(list))
	for i, s := range list {
		out[i] = SessionInfo{Session: s, Current: s.ID != "" && s.ID == actor.SessionID}
	}
	return out, nil
}

// RevokeSession signs out one of the user's own sessions.
func (a *App) RevokeSession(ctx context.Context, actor Actor, id string) error {
	if err := actor.interactive(); err != nil {
		return err
	}
	if err := a.Store.DeleteSessionByID(ctx, actor.User.ID, id); err != nil {
		return err
	}
	a.audit(ctx, actor, "auth.session_revoke", id, nil)
	return nil
}

// TOTPEnrollment is what the user needs to add the account to an
// authenticator app.
type TOTPEnrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// BeginTOTP generates a secret for the user. Two-factor sign-in is not
// active until ConfirmTOTP proves the authenticator has it.
func (a *App) BeginTOTP(ctx context.Context, actor Actor) (TOTPEnrollment, error) {
	u, err := a.self(ctx, actor)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	if u.TOTPEnabled {
		return TOTPEnrollment{}, conflict("two-factor sign-in is already on; turn it off first to set up a new device")
	}
	secret := secure.NewTOTPSecret()
	sealed, err := secure.Seal(a.keyring, []byte(secret), totpAAD(u.ID))
	if err != nil {
		return TOTPEnrollment{}, err
	}
	if err := a.Store.SetTOTP(ctx, u.ID, sealed, false, nil); err != nil {
		return TOTPEnrollment{}, err
	}
	return TOTPEnrollment{Secret: secret, URI: secure.TOTPURI("Dedie", u.Email, secret)}, nil
}

// ConfirmTOTP turns two-factor sign-in on once the user enters a valid
// code, and returns one-time recovery codes. They are shown this once.
func (a *App) ConfirmTOTP(ctx context.Context, actor Actor, code string) ([]string, error) {
	u, err := a.self(ctx, actor)
	if err != nil {
		return nil, err
	}
	if u.TOTPEnabled {
		return nil, conflict("two-factor sign-in is already on")
	}
	secret, err := secure.Open(a.keyring, u.TOTPSecretEnc, totpAAD(u.ID))
	if err != nil || len(secret) == 0 {
		return nil, conflict("start two-factor setup first")
	}
	if !secure.VerifyTOTP(string(secret), code, time.Now()) {
		return nil, invalid("that code is not valid; check that the clock on your phone is correct")
	}
	codes := make([]string, 8)
	hashes := make([]string, len(codes))
	for i := range codes {
		codes[i] = secure.NewRecoveryCode()
		hashes[i] = secure.HashToken(codes[i])
	}
	if err := a.Store.SetTOTP(ctx, u.ID, u.TOTPSecretEnc, true, hashes); err != nil {
		return nil, err
	}
	// Sessions opened with the password alone end now.
	a.Store.DeleteOtherSessions(ctx, u.ID, actor.SessionID)
	a.audit(ctx, actor, "auth.totp_enable", u.ID, nil)
	return codes, nil
}

// DisableTOTP turns two-factor sign-in off. It asks for the password, so a
// borrowed session cannot quietly weaken the account.
func (a *App) DisableTOTP(ctx context.Context, actor Actor, password string) error {
	u, err := a.self(ctx, actor)
	if err != nil {
		return err
	}
	if !secure.VerifyPassword(u.PasswordHash, password) {
		return &Error{Status: 403, Message: "the password is not correct"}
	}
	if a.policy(ctx, u.OrgID).RequireTOTP {
		return conflict("your organization requires two-factor sign-in")
	}
	if err := a.Store.SetTOTP(ctx, u.ID, nil, false, nil); err != nil {
		return err
	}
	a.audit(ctx, actor, "auth.totp_disable", u.ID, nil)
	return nil
}
