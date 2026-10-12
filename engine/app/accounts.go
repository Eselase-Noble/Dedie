package app

import (
	"context"
	"time"

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

// adminProduction is admin for an action that takes something away from
// the server's users: on a production connection it also wants the
// connection's name typed.
func (a *App) adminProduction(ctx context.Context, actor Actor, connID, action, what string) (string, string, connector.Session, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return "", "", nil, err
	}
	c, sess, err := a.writableProduction(ctx, actor, connID, action, what)
	return c.ID, c.Name, sess, err
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
	id, name, sess, err := a.adminProduction(ctx, actor, connID, "dbuser.drop", "drop one of its users")
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

// Terminated is the outcome of disconnecting sessions.
type Terminated struct {
	// Sessions is how many were disconnected.
	Sessions int `json:"sessions"`
	// OpenNow is how many sessions of the same user and state exist a
	// moment later, or -1 when that was not looked at. An application
	// that keeps a connection pool opens new connections as soon as it
	// loses some, so the number an administrator sees afterwards can be
	// what it was before although every session asked for was closed.
	OpenNow int `json:"open_now"`
}

// reopenWait is how long an application is given to reconnect before
// its sessions are counted again.
const reopenWait = 1500 * time.Millisecond

// TerminateSessions disconnects every session of a user, an application,
// a client address or a database, and reports how many there were.
func (a *App) TerminateSessions(ctx context.Context, actor Actor, connID string, filter connector.SessionFilter) (Terminated, error) {
	out := Terminated{OpenNow: -1}
	if filter.Empty() {
		return out, invalid("say whose sessions to disconnect")
	}
	id, name, sess, err := a.adminProduction(ctx, actor, connID, "server.terminate_sessions", "disconnect its sessions")
	if err != nil {
		return out, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	n, err := sess.TerminateSessions(ctx, filter)
	if err != nil {
		return out, &Error{Status: 422, Message: err.Error()}
	}
	out.Sessions = n
	a.audit(ctx, actor, "server.terminate_sessions", id, map[string]any{"connection": name, "filter": filter, "sessions": n})
	if n > 0 && filter.Application == "" && filter.Database == "" && filter.Client == "" {
		select {
		case <-ctx.Done():
			return out, nil
		case <-time.After(reopenWait):
		}
		if acc, err := sess.Accounts(ctx); err == nil {
			out.OpenNow = 0
			for _, u := range acc.Accounts {
				if filter.User != "" && u.Name != filter.User {
					continue
				}
				switch filter.State {
				case connector.StateIdle:
					out.OpenNow += u.Idle
				case connector.StateIdleInTransaction:
					out.OpenNow += u.IdleInTransaction
				default:
					out.OpenNow += u.Connections
				}
			}
		}
	}
	return out, nil
}
