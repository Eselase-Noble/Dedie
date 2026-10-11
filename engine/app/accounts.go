package app

import (
	"context"

	"dedie/engine/connector"
)

// DatabaseAccounts returns the users, roles, connection limits and current
// connection usage of a connection's server.
func (a *App) DatabaseAccounts(ctx context.Context, actor Actor, connID string) (*connector.Accounts, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return nil, err
	}
	_, sess, err := a.open(ctx, actor, connID)
	if err != nil {
		return nil, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	out, err := sess.Accounts(ctx)
	if err != nil {
		return nil, &Error{Status: 422, Message: err.Error()}
	}
	return out, nil
}

// admin opens a connection for a change to the server's users or
// sessions: the actor must administer servers and be allowed to write, and
// the connection must not be read-only.
func (a *App) admin(ctx context.Context, actor Actor, connID string) (string, string, connector.Session, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return "", "", nil, err
	}
	c, sess, err := a.writable(ctx, actor, connID)
	return c.ID, c.Name, sess, err
}

// SaveDatabaseAccount creates a database user or changes one. The audit
// entry records what was changed, never the password.
func (a *App) SaveDatabaseAccount(ctx context.Context, actor Actor, connID string, change connector.AccountChange) error {
	id, name, sess, err := a.admin(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	action := "dbuser.update"
	if change.Create {
		action = "dbuser.create"
	}
	detail := map[string]any{"connection": name, "user": change.Name, "password_set": change.Password != nil && *change.Password != ""}
	if change.Host != "" {
		detail["host"] = change.Host
	}
	if change.CanLogin != nil {
		detail["can_login"] = *change.CanLogin
	}
	if change.ConnectionLimit != nil {
		detail["connection_limit"] = *change.ConnectionLimit
	}
	if len(change.Grant) > 0 {
		detail["grant"] = change.Grant
	}
	if len(change.Revoke) > 0 {
		detail["revoke"] = change.Revoke
	}
	if change.Access != "" {
		detail["access"] = change.Access
	}
	if err := sess.SaveAccount(ctx, change); err != nil {
		detail["error"] = err.Error()
		a.audit(ctx, actor, action, id, detail)
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, action, id, detail)
	return nil
}

func (a *App) DropDatabaseAccount(ctx context.Context, actor Actor, connID, user, host string, handOver bool) error {
	id, name, sess, err := a.admin(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.DropAccount(ctx, user, host, handOver); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "dbuser.drop", id, map[string]any{"connection": name, "user": user, "host": host, "hand_over": handOver})
	return nil
}

// SetConnectionLimit caps the connections a database user or a database
// may hold.
func (a *App) SetConnectionLimit(ctx context.Context, actor Actor, connID string, limit connector.LimitChange) error {
	id, name, sess, err := a.admin(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.SetConnectionLimit(ctx, limit); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "server.connection_limit", id, map[string]any{"connection": name, "scope": limit.Scope, "name": limit.Name, "limit": limit.Limit})
	return nil
}

// TerminateSessions disconnects every session of a user, an application,
// a client address or a database, and reports how many there were.
func (a *App) TerminateSessions(ctx context.Context, actor Actor, connID string, filter connector.SessionFilter) (int, error) {
	if filter.Empty() {
		return 0, invalid("say whose sessions to disconnect")
	}
	id, name, sess, err := a.admin(ctx, actor, connID)
	if err != nil {
		return 0, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	n, err := sess.TerminateSessions(ctx, filter)
	if err != nil {
		return 0, &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "server.terminate_sessions", id, map[string]any{"connection": name, "filter": filter, "sessions": n})
	return n, nil
}
