package app

import (
	"context"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/store"
)

// selfApplication is the application name Dedie's own PostgreSQL sessions
// carry. Automatic closing never touches them: a migration's sessions sit
// idle between batches of changes.
const selfApplication = "dedie"

// GetIdlePolicy returns a connection's rule for closing idle connections.
func (a *App) GetIdlePolicy(ctx context.Context, actor Actor, connID string) (store.IdlePolicy, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return store.IdlePolicy{}, err
	}
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return store.IdlePolicy{}, err
	}
	return a.Store.GetIdlePolicy(ctx, c.OrgID, c.ID)
}

// SetIdlePolicy saves a connection's rule. Once enabled, the server
// applies it every minute.
func (a *App) SetIdlePolicy(ctx context.Context, actor Actor, connID string, in store.IdlePolicy) (store.IdlePolicy, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return in, err
	}
	if err := actor.require(PermQueryWrite); err != nil {
		return in, err
	}
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return in, err
	}
	if c.ReadOnly {
		return in, &Error{Status: 403, Message: "this connection is marked read-only"}
	}
	if c.Kind != "postgres" && c.Kind != "mysql" {
		return in, invalid("closing idle connections is available for PostgreSQL and MySQL")
	}
	if in.IdleMinutes < 1 || in.IdleMinutes > 7*24*60 {
		return in, invalid("the idle time must be between 1 minute and 7 days")
	}
	if in.TxMinutes < 0 || in.MinUsagePercent < 0 || in.MinUsagePercent > 100 {
		return in, invalid("the values are out of range")
	}
	if c.Kind == "mysql" {
		in.TxMinutes = 0
	}
	var exclude []string
	for _, u := range in.ExcludeUsers {
		if u = strings.TrimSpace(u); u != "" {
			exclude = append(exclude, u)
		}
	}
	p := store.IdlePolicy{ConnectionID: c.ID, OrgID: c.OrgID, Enabled: in.Enabled, IdleMinutes: in.IdleMinutes, TxMinutes: in.TxMinutes,
		MinUsagePercent: in.MinUsagePercent, ExcludeUsers: exclude, UpdatedBy: actor.User.Email}
	if err := a.Store.SaveIdlePolicy(ctx, p); err != nil {
		return p, err
	}
	a.audit(ctx, actor, "server.idle_policy", c.ID, map[string]any{"connection": c.Name, "enabled": p.Enabled, "idle_minutes": p.IdleMinutes,
		"tx_minutes": p.TxMinutes, "min_usage_percent": p.MinUsagePercent, "exclude_users": p.ExcludeUsers})
	return a.Store.GetIdlePolicy(ctx, c.OrgID, c.ID)
}

// enforceIdlePolicies applies every enabled rule once. The monitor calls
// it each minute.
func (a *App) enforceIdlePolicies(ctx context.Context) {
	policies, err := a.Store.EnabledIdlePolicies(ctx)
	if err != nil {
		return
	}
	for _, p := range policies {
		closed, err := a.enforceIdlePolicy(ctx, p)
		if ctx.Err() != nil {
			return
		}
		text := ""
		if err != nil {
			text = err.Error()
		}
		a.Store.RecordIdleRun(ctx, p.ConnectionID, closed, text)
	}
}

func (a *App) enforceIdlePolicy(ctx context.Context, p store.IdlePolicy) (int, error) {
	c, err := a.Store.GetConnection(ctx, p.OrgID, p.ConnectionID)
	if err != nil {
		return 0, err
	}
	cfg, err := a.config(c)
	if err != nil {
		return 0, err
	}
	drv, err := connector.Get(c.Kind)
	if err != nil {
		return 0, err
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer sess.Close(context.WithoutCancel(ctx))

	if p.MinUsagePercent > 0 {
		acc, err := sess.Accounts(ctx)
		if err != nil {
			return 0, err
		}
		room := acc.MaxConnections - acc.Reserved
		if room <= 0 || acc.Used*100/room < p.MinUsagePercent {
			return 0, nil // the server has room; leave everyone connected
		}
	}
	// The account this connection uses is spared as well: it is the one
	// Dedie's own work runs under.
	filter := connector.SessionFilter{State: connector.StateIdle, IdleSeconds: int64(p.IdleMinutes) * 60,
		ExcludeUsers: append([]string{c.Username}, p.ExcludeUsers...), ExcludeApplications: []string{selfApplication}}
	closed, err := sess.TerminateSessions(ctx, filter)
	if err != nil {
		return 0, err
	}
	if p.TxMinutes > 0 && c.Kind == "postgres" {
		filter.State, filter.IdleSeconds = connector.StateIdleInTransaction, int64(p.TxMinutes)*60
		n, err := sess.TerminateSessions(ctx, filter)
		if err != nil {
			return closed, err
		}
		closed += n
	}
	if closed > 0 {
		a.Store.Audit(context.WithoutCancel(ctx), c.OrgID, "system", "server.idle_closed", c.ID,
			fmt.Sprintf(`{"connection":%q,"sessions":%d,"idle_minutes":%d}`, c.Name, closed, p.IdleMinutes), "")
	}
	return closed, nil
}
