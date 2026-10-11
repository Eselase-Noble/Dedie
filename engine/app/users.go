package app

import (
	"context"
	"strings"

	"dedie/engine/secure"
	"dedie/engine/store"
)

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
	if err := a.policy(ctx, actor.User.OrgID).checkPassword(password); err != nil {
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
	// ResetTOTP turns off two-factor sign-in for a user who has lost
	// their authenticator and recovery codes.
	ResetTOTP bool `json:"reset_totp"`
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
		if err := a.policy(ctx, actor.User.OrgID).checkPassword(*up.Password); err != nil {
			return u, err
		}
		if u.PasswordHash, err = secure.HashPassword(*up.Password); err != nil {
			return u, err
		}
		changed["password"] = "changed"
	}
	if up.ResetTOTP && (u.TOTPEnabled || len(u.TOTPSecretEnc) > 0) {
		if err := a.Store.SetTOTP(ctx, u.ID, nil, false, nil); err != nil {
			return u, err
		}
		u.TOTPEnabled = false
		changed["two_factor"] = "reset"
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
