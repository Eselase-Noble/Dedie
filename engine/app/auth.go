package app

import (
	"context"
	"errors"
	"net/mail"
	"os"
	"os/user"
	"strings"
	"time"

	"dedie/engine/secure"
	"dedie/engine/store"
)

const (
	maxLoginFails = 5
	lockoutPeriod = 15 * time.Minute
	// tokenPrefix marks API tokens, so that one pasted into the wrong
	// place, or found in a log, is recognizable for what it is.
	tokenPrefix = "dedie_pat_"
)

func checkEmail(email string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil {
		return "", invalid("enter a valid email address")
	}
	return strings.ToLower(addr.Address), nil
}

// NeedsSetup reports whether the first organization has yet to be created.
func (a *App) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := a.Store.CountOrgs(ctx)
	return n == 0, err
}

// Setup creates the first organization and its owner. It works only once.
func (a *App) Setup(ctx context.Context, orgName, name, email, password string) (store.User, error) {
	need, err := a.NeedsSetup(ctx)
	if err != nil {
		return store.User{}, err
	}
	if !need {
		return store.User{}, conflict("this installation is already set up")
	}
	if strings.TrimSpace(orgName) == "" || strings.TrimSpace(name) == "" {
		return store.User{}, invalid("organization and name are required")
	}
	if email, err = checkEmail(email); err != nil {
		return store.User{}, err
	}
	if err := defaultPolicy.checkPassword(password); err != nil {
		return store.User{}, err
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	org := store.Org{ID: secure.NewID("org"), Name: strings.TrimSpace(orgName)}
	if err := a.Store.CreateOrg(ctx, org); err != nil {
		return store.User{}, err
	}
	u := store.User{ID: secure.NewID("usr"), OrgID: org.ID, Email: email, Name: strings.TrimSpace(name), PasswordHash: hash, Role: RoleOwner}
	if err := a.Store.CreateUser(ctx, u); err != nil {
		return store.User{}, err
	}
	a.audit(ctx, Actor{User: u}, "org.setup", org.ID, map[string]string{"organization": org.Name})
	return u, nil
}

// LoginInput is a sign-in attempt. Code is the authenticator or recovery
// code, needed only when the account has two-factor sign-in.
type LoginInput struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	Code      string `json:"code"`
	IP        string `json:"-"`
	UserAgent string `json:"-"`
}

// LoginResult is a new session.
type LoginResult struct {
	Token string
	User  store.User
	TTL   time.Duration
	// MustEnroll tells the client to send the user to two-factor setup.
	MustEnroll bool
}

// Login verifies credentials and returns a new session. The response is the
// same for an unknown email and a wrong password, and repeated failures
// lock the account for a while to slow down guessing.
func (a *App) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	bad := &Error{Status: 401, Message: "incorrect email or password"}
	u, err := a.Store.UserByEmail(ctx, strings.TrimSpace(in.Email))
	if errors.Is(err, store.ErrNotFound) {
		// Spend the same time as a real check so timing does not reveal
		// which emails exist.
		secure.VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", in.Password)
		return LoginResult{}, bad
	}
	if err != nil {
		return LoginResult{}, err
	}
	actor := Actor{User: u, IP: in.IP}
	if time.Now().Before(u.LockedUntil) {
		a.audit(ctx, actor, "auth.login_blocked", u.ID, nil)
		return LoginResult{}, &Error{Status: 429, Message: "too many failed attempts; try again later"}
	}
	if u.Disabled || !secure.VerifyPassword(u.PasswordHash, in.Password) {
		a.Store.RecordLogin(ctx, u.ID, false, maxLoginFails, lockoutPeriod)
		a.audit(ctx, actor, "auth.login_failed", u.ID, nil)
		return LoginResult{}, bad
	}
	if u.TOTPEnabled {
		if strings.TrimSpace(in.Code) == "" {
			// The password was right; ask for the second factor without
			// counting this as a failure.
			return LoginResult{}, &Error{Status: 401, Code: "totp_required", Message: "enter the code from your authenticator app"}
		}
		if !a.checkSecondFactor(ctx, &u, in.Code) {
			a.Store.RecordLogin(ctx, u.ID, false, maxLoginFails, lockoutPeriod)
			a.audit(ctx, actor, "auth.login_failed", u.ID, map[string]string{"reason": "second factor"})
			return LoginResult{}, &Error{Status: 401, Code: "totp_invalid", Message: "that code is not valid; check the clock on your phone or use a recovery code"}
		}
	}
	a.Store.RecordLogin(ctx, u.ID, true, maxLoginFails, lockoutPeriod)
	a.Store.PurgeExpiredSessions(ctx)
	// The sessions still open say where this account is normally used
	// from. Nothing more is kept, so a first sign-in, or one after signing
	// out everywhere, has nothing to be compared with.
	known, _ := a.Store.LiveSessionAddresses(ctx, u.ID)
	policy := a.policy(ctx, u.OrgID)
	token := secure.NewToken(32)
	// Only a hash of the token is stored: a copy of the database does not
	// yield usable sessions.
	if err := a.Store.CreateSession(ctx, secure.NewID("ses"), secure.HashToken(token), u.ID, in.IP, clip(in.UserAgent, 300), policy.sessionTTL()); err != nil {
		return LoginResult{}, err
	}
	a.audit(ctx, actor, "auth.login", u.ID, nil)
	if in.IP != "" && len(known) > 0 && !contains(known, in.IP) {
		a.Notify(ctx, store.Notification{OrgID: u.OrgID, UserID: u.ID, Kind: NoteNewSignIn, Level: LevelWarning, Title: "New sign-in from " + in.IP,
			Body: "Your account was signed in to from an address none of your other sessions use. End the session and change your password if this was not you.", Link: accountLink})
	}
	return LoginResult{Token: token, User: u, TTL: policy.sessionTTL(), MustEnroll: policy.RequireTOTP && !u.TOTPEnabled}, nil
}

// checkSecondFactor accepts the current authenticator code or an unused
// recovery code, which it then consumes.
func (a *App) checkSecondFactor(ctx context.Context, u *store.User, code string) bool {
	secret, err := secure.Open(a.keyring, u.TOTPSecretEnc, totpAAD(u.ID))
	if err != nil {
		return false
	}
	if secure.VerifyTOTP(string(secret), code, time.Now()) {
		return true
	}
	hash := secure.HashToken(strings.ToLower(strings.TrimSpace(code)))
	for i, have := range u.RecoveryCodes {
		if have == hash {
			u.RecoveryCodes = append(u.RecoveryCodes[:i:i], u.RecoveryCodes[i+1:]...)
			a.Store.SetTOTP(ctx, u.ID, u.TOTPSecretEnc, true, u.RecoveryCodes)
			a.audit(ctx, Actor{User: *u}, "auth.recovery_code_used", u.ID, map[string]int{"remaining": len(u.RecoveryCodes)})
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, have := range list {
		if have == v {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Authenticate resolves a session token to its user.
func (a *App) Authenticate(ctx context.Context, token, ip string) (Actor, error) {
	if token == "" {
		return Actor{}, ErrUnauthenticated
	}
	u, sessionID, err := a.Store.SessionUser(ctx, secure.HashToken(token))
	if err != nil {
		return Actor{}, ErrUnauthenticated
	}
	return Actor{User: u, IP: ip, SessionID: sessionID, MustEnroll: a.policy(ctx, u.OrgID).RequireTOTP && !u.TOTPEnabled}, nil
}

// AuthenticateToken resolves an API token to its owner. The actor never
// has more than the token's role, nor more than the owner has today.
func (a *App) AuthenticateToken(ctx context.Context, token, ip string) (Actor, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Actor{}, ErrUnauthenticated
	}
	t, err := a.Store.TokenByHash(ctx, secure.HashToken(token))
	if err != nil {
		return Actor{}, ErrUnauthenticated
	}
	u, err := a.Store.UserByID(ctx, t.OrgID, t.UserID)
	if err != nil || u.Disabled {
		return Actor{}, ErrUnauthenticated
	}
	if roleRank[t.Role] < roleRank[u.Role] {
		u.Role = t.Role
	}
	return Actor{User: u, IP: ip, Token: t.Name, TokenID: t.ID}, nil
}

func (a *App) Logout(ctx context.Context, actor Actor, token string) {
	a.Store.DeleteSession(ctx, secure.HashToken(token))
	a.audit(ctx, actor, "auth.logout", actor.User.ID, nil)
}

// LocalActor is the identity used by the CLI in local mode, which runs
// with direct access to the data directory and master key and is therefore
// trusted as an owner. Its actions are audited under the operating-system
// user name.
func (a *App) LocalActor(ctx context.Context) (Actor, error) {
	org, err := a.Store.FirstOrg(ctx)
	if err != nil {
		return Actor{}, errors.New("no organization yet; run `dedie init` first")
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return Actor{User: store.User{ID: "cli", OrgID: org.ID, Email: "cli:" + name, Name: name, Role: RoleOwner}, IP: "local"}, nil
}

func (a *App) Organization(ctx context.Context, actor Actor) (store.Org, error) {
	return a.Store.GetOrg(ctx, actor.User.OrgID)
}
