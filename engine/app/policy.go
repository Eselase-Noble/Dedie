package app

import (
	"context"
	"encoding/json"
	"time"
	"unicode"
)

// SecurityPolicy holds an organization's sign-in rules.
type SecurityPolicy struct {
	PasswordMinLength int `json:"password_min_length"`
	// PasswordRequireMixed demands upper and lower case letters, a digit
	// and a symbol.
	PasswordRequireMixed bool `json:"password_require_mixed"`
	// SessionHours is how long a sign-in lasts.
	SessionHours int `json:"session_hours"`
	// RequireTOTP makes every user set up two-factor sign-in before they
	// can do anything else.
	RequireTOTP bool `json:"require_totp"`
}

var defaultPolicy = SecurityPolicy{PasswordMinLength: 12, SessionHours: 12}

const policyKey = "security_policy"

func (p SecurityPolicy) sessionTTL() time.Duration { return time.Duration(p.SessionHours) * time.Hour }

func (p SecurityPolicy) checkPassword(pw string) error {
	if len(pw) < p.PasswordMinLength {
		return invalid("password must be at least %d characters", p.PasswordMinLength)
	}
	if len(pw) > 256 {
		return invalid("password must be at most 256 characters")
	}
	if !p.PasswordRequireMixed {
		return nil
	}
	var upper, lower, digit, symbol bool
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		default:
			symbol = true
		}
	}
	if !(upper && lower && digit && symbol) {
		return invalid("password must contain upper and lower case letters, a digit and a symbol")
	}
	return nil
}

// policy returns the organization's policy, falling back to the defaults
// for anything unset or unreadable.
func (a *App) policy(ctx context.Context, orgID string) SecurityPolicy {
	p := defaultPolicy
	if raw, err := a.Store.Setting(ctx, orgID, policyKey); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// GetSecurityPolicy is readable by every signed-in user: the account page
// shows the password rules.
func (a *App) GetSecurityPolicy(ctx context.Context, actor Actor) SecurityPolicy {
	return a.policy(ctx, actor.User.OrgID)
}

func (a *App) SetSecurityPolicy(ctx context.Context, actor Actor, p SecurityPolicy) (SecurityPolicy, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return p, err
	}
	if err := actor.interactive(); err != nil {
		return p, err
	}
	if p.PasswordMinLength < 8 || p.PasswordMinLength > 128 {
		return p, invalid("minimum password length must be between 8 and 128")
	}
	if p.SessionHours < 1 || p.SessionHours > 24*30 {
		return p, invalid("session length must be between 1 hour and 30 days")
	}
	raw, _ := json.Marshal(p)
	if err := a.Store.SetSetting(ctx, actor.User.OrgID, policyKey, string(raw)); err != nil {
		return p, err
	}
	a.audit(ctx, actor, "security.policy", actor.User.OrgID, p)
	return p, nil
}
