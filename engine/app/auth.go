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
	sessionTTL     = 12 * time.Hour
	maxLoginFails  = 5
	lockoutPeriod  = 15 * time.Minute
	minPasswordLen = 12
)

// SessionTTL is how long a sign-in lasts.
func SessionTTL() time.Duration { return sessionTTL }

func checkPassword(pw string) error {
	if len(pw) < minPasswordLen {
		return invalid("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

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
	if err := checkPassword(password); err != nil {
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

// Login verifies credentials and returns a new session token. The response is
// the same for an unknown email and a wrong password, and repeated failures
// lock the account for a while to slow down guessing.
func (a *App) Login(ctx context.Context, email, password, ip string) (string, store.User, error) {
	bad := &Error{Status: 401, Message: "incorrect email or password"}
	u, err := a.Store.UserByEmail(ctx, strings.TrimSpace(email))
	if errors.Is(err, store.ErrNotFound) {
		// Spend the same time as a real check so timing does not reveal
		// which emails exist.
		secure.VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return "", u, bad
	}
	if err != nil {
		return "", u, err
	}
	actor := Actor{User: u, IP: ip}
	if time.Now().Before(u.LockedUntil) {
		a.audit(ctx, actor, "auth.login_blocked", u.ID, nil)
		return "", u, &Error{Status: 429, Message: "too many failed attempts; try again later"}
	}
	if u.Disabled || !secure.VerifyPassword(u.PasswordHash, password) {
		a.Store.RecordLogin(ctx, u.ID, false, maxLoginFails, lockoutPeriod)
		a.audit(ctx, actor, "auth.login_failed", u.ID, nil)
		return "", u, bad
	}
	a.Store.RecordLogin(ctx, u.ID, true, maxLoginFails, lockoutPeriod)
	a.Store.PurgeExpiredSessions(ctx)
	token := secure.NewToken(32)
	// Only a hash of the token is stored: a copy of the database does not
	// yield usable sessions.
	if err := a.Store.CreateSession(ctx, secure.HashToken(token), u.ID, ip, sessionTTL); err != nil {
		return "", u, err
	}
	a.audit(ctx, actor, "auth.login", u.ID, nil)
	return token, u, nil
}

// Authenticate resolves a session token to its user.
func (a *App) Authenticate(ctx context.Context, token, ip string) (Actor, error) {
	if token == "" {
		return Actor{}, ErrUnauthenticated
	}
	u, err := a.Store.SessionUser(ctx, secure.HashToken(token))
	if err != nil {
		return Actor{}, ErrUnauthenticated
	}
	return Actor{User: u, IP: ip}, nil
}

func (a *App) Logout(ctx context.Context, actor Actor, token string) {
	a.Store.DeleteSession(ctx, secure.HashToken(token))
	a.audit(ctx, actor, "auth.logout", actor.User.ID, nil)
}

// LocalActor is the identity used by the CLI, which runs with direct access
// to the data directory and master key and is therefore trusted as an owner.
// Its actions are audited under the operating-system user name.
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

// --- user management ---

func (a *App) ListUsers(ctx context.Context, actor Actor) ([]store.User, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return nil, err
	}
	return a.Store.ListUsers(ctx, actor.User.OrgID)
}

func (a *App) CreateUser(ctx context.Context, actor Actor, name, email, password, role string) (store.User, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return store.User{}, err
	}
	if !validRole(role) {
		return store.User{}, invalid("unknown role %q", role)
	}
	// Only an owner may create another owner.
	if role == RoleOwner && actor.User.Role != RoleOwner {
		return store.User{}, ErrForbidden
	}
	if strings.TrimSpace(name) == "" {
		return store.User{}, invalid("name is required")
	}
	email, err := checkEmail(email)
	if err != nil {
		return store.User{}, err
	}
	if err := checkPassword(password); err != nil {
		return store.User{}, err
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	u := store.User{ID: secure.NewID("usr"), OrgID: actor.User.OrgID, Email: email, Name: strings.TrimSpace(name), PasswordHash: hash, Role: role}
	if err := a.Store.CreateUser(ctx, u); err != nil {
		return store.User{}, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, "user.create", u.ID, map[string]string{"email": email, "role": role})
	return u, nil
}

// UserUpdate holds the fields an administrator may change; nil means unchanged.
type UserUpdate struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Password *string `json:"password"`
}

func (a *App) UpdateUser(ctx context.Context, actor Actor, id string, up UserUpdate) (store.User, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return store.User{}, err
	}
	u, err := a.Store.UserByID(ctx, actor.User.OrgID, id)
	if err != nil {
		return u, wrap(err)
	}
	// Owners can only be changed by owners.
	if u.Role == RoleOwner && actor.User.Role != RoleOwner {
		return u, ErrForbidden
	}
	wasActiveOwner := u.Role == RoleOwner && !u.Disabled
	changed := map[string]any{}
	if up.Name != nil && strings.TrimSpace(*up.Name) != "" {
		u.Name = strings.TrimSpace(*up.Name)
		changed["name"] = u.Name
	}
	if up.Role != nil && *up.Role != u.Role {
		if !validRole(*up.Role) {
			return u, invalid("unknown role %q", *up.Role)
		}
		if *up.Role == RoleOwner && actor.User.Role != RoleOwner {
			return u, ErrForbidden
		}
		u.Role = *up.Role
		changed["role"] = u.Role
	}
	if up.Disabled != nil && *up.Disabled != u.Disabled {
		if *up.Disabled && u.ID == actor.User.ID {
			return u, invalid("you cannot disable your own account")
		}
		u.Disabled = *up.Disabled
		changed["disabled"] = u.Disabled
	}
	if up.Password != nil && *up.Password != "" {
		if err := checkPassword(*up.Password); err != nil {
			return u, err
		}
		if u.PasswordHash, err = secure.HashPassword(*up.Password); err != nil {
			return u, err
		}
		changed["password"] = "changed"
	}
	// An organization must always keep one owner who can sign in.
	if wasActiveOwner && (u.Role != RoleOwner || u.Disabled) {
		n, err := a.Store.CountActiveOwners(ctx, actor.User.OrgID)
		if err != nil {
			return u, err
		}
		if n <= 1 {
			return u, conflict("the organization must keep at least one active owner")
		}
	}
	if err := a.Store.UpdateUser(ctx, u); err != nil {
		return u, err
	}
	// A changed password, role or status ends the user's existing sessions.
	if len(changed) > 0 {
		if _, onlyName := changed["name"]; !(onlyName && len(changed) == 1) {
			a.Store.DeleteUserSessions(ctx, u.ID)
		}
	}
	a.audit(ctx, actor, "user.update", u.ID, changed)
	return u, nil
}
