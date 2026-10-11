package app

import (
	"context"
	"strings"
	"time"

	"dedie/engine/secure"
	"dedie/engine/store"
)

// TokenInput describes an API token to create.
type TokenInput struct {
	Name string `json:"name"`
	// Role caps what the token may do. It cannot exceed the creator's own
	// role; empty means the same role.
	Role string `json:"role"`
	// Days until the token expires; 0 means it does not.
	Days int `json:"days"`
}

// CreatedToken carries the token text, which is shown once and never
// stored.
type CreatedToken struct {
	store.APIToken
	Token string `json:"token"`
}

// CreateToken issues an API token for the signed-in user.
func (a *App) CreateToken(ctx context.Context, actor Actor, in TokenInput) (CreatedToken, error) {
	if err := actor.interactive(); err != nil {
		return CreatedToken{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 80 {
		return CreatedToken{}, invalid("give the token a name of up to 80 characters")
	}
	if in.Role == "" {
		in.Role = actor.User.Role
	}
	if !validRole(in.Role) {
		return CreatedToken{}, invalid("unknown role %q", in.Role)
	}
	if roleRank[in.Role] > roleRank[actor.User.Role] {
		return CreatedToken{}, &Error{Status: 403, Message: "a token cannot have a higher role than your own"}
	}
	if in.Days < 0 || in.Days > 3650 {
		return CreatedToken{}, invalid("expiry must be between 0 and 3650 days")
	}
	t := store.APIToken{ID: secure.NewID("tok"), OrgID: actor.User.OrgID, UserID: actor.User.ID, UserEmail: actor.User.Email,
		Name: in.Name, Role: in.Role, CreatedAt: time.Now().UTC()}
	if in.Days > 0 {
		t.ExpiresAt = time.Now().AddDate(0, 0, in.Days).UTC()
	}
	token := tokenPrefix + secure.NewToken(32)
	if err := a.Store.CreateAPIToken(ctx, t, secure.HashToken(token)); err != nil {
		return CreatedToken{}, err
	}
	a.audit(ctx, actor, "token.create", t.ID, map[string]any{"name": t.Name, "role": t.Role, "days": in.Days})
	return CreatedToken{APIToken: t, Token: token}, nil
}

// ListTokens lists the caller's tokens; administrators see everyone's.
func (a *App) ListTokens(ctx context.Context, actor Actor) ([]store.APIToken, error) {
	userID := actor.User.ID
	if actor.Can(PermManageUsers) {
		userID = ""
	}
	return a.Store.ListAPITokens(ctx, actor.User.OrgID, userID)
}

// RevokeToken deletes a token: the caller's own, or anyone's for an
// administrator.
func (a *App) RevokeToken(ctx context.Context, actor Actor, id string) error {
	if err := actor.interactive(); err != nil {
		return err
	}
	t, err := a.Store.GetAPIToken(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if t.UserID != actor.User.ID && !actor.Can(PermManageUsers) {
		return ErrForbidden
	}
	if err := a.Store.DeleteAPIToken(ctx, t.OrgID, t.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "token.revoke", t.ID, map[string]string{"name": t.Name, "owner": t.UserEmail})
	return nil
}
