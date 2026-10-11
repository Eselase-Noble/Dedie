package app

import (
	"context"
	"strings"
	"time"

	"dedie/engine/connector"
)

// QueryRequest is one execution from the query browser.
type QueryRequest struct {
	SQL string `json:"sql"`
	// Write asks for a read-write session. Without it the statement runs in
	// a read-only transaction.
	Write   bool `json:"write"`
	MaxRows int  `json:"max_rows"`
}

type QueryResponse struct {
	Results    []connector.ResultSet `json:"results"`
	ReadOnly   bool                  `json:"read_only"`
	DurationMS int64                 `json:"duration_ms"`
}

// Query runs SQL on a saved connection. Every execution is audited with the
// statement text, so there is a record of who ran what and where.
func (a *App) Query(ctx context.Context, actor Actor, connID string, req QueryRequest) (QueryResponse, error) {
	var resp QueryResponse
	if err := actor.require(PermQueryRead); err != nil {
		return resp, err
	}
	if strings.TrimSpace(req.SQL) == "" {
		return resp, invalid("enter a statement to run")
	}
	if req.MaxRows <= 0 || req.MaxRows > 10000 {
		req.MaxRows = 1000
	}
	c, sess, err := a.open(ctx, actor.User.OrgID, connID)
	if err != nil {
		return resp, err
	}
	defer sess.Close(context.WithoutCancel(ctx))

	if req.Write {
		if !actor.Can(PermQueryWrite) {
			return resp, &Error{Status: 403, Message: "your role allows read-only queries only"}
		}
		if c.ReadOnly {
			return resp, &Error{Status: 403, Message: "this connection is marked read-only"}
		}
	}
	resp.ReadOnly = !req.Write

	stmt := req.SQL
	if len(stmt) > 2000 {
		stmt = stmt[:2000] + "..."
	}
	start := time.Now()
	resp.Results, err = sess.Query(ctx, req.SQL, connector.QueryOptions{ReadOnly: resp.ReadOnly, MaxRows: req.MaxRows, TimeoutSeconds: 120})
	resp.DurationMS = time.Since(start).Milliseconds()
	detail := map[string]any{"connection": c.Name, "sql": stmt, "write": req.Write, "ms": resp.DurationMS}
	if err != nil {
		detail["error"] = err.Error()
		a.audit(ctx, actor, "query.execute", c.ID, detail)
		return resp, &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "query.execute", c.ID, detail)
	if resp.Results == nil {
		resp.Results = []connector.ResultSet{}
	}
	return resp, nil
}

// Catalog returns the schema tree of a connection.
func (a *App) Catalog(ctx context.Context, actor Actor, connID string) (*connector.Catalog, error) {
	if err := actor.require(PermQueryRead); err != nil {
		return nil, err
	}
	_, sess, err := a.open(ctx, actor.User.OrgID, connID)
	if err != nil {
		return nil, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	return sess.Catalog(ctx)
}

// ServerOverview returns the health snapshot for the administration screen.
func (a *App) ServerOverview(ctx context.Context, actor Actor, connID string) (*connector.Overview, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return nil, err
	}
	_, sess, err := a.open(ctx, actor.User.OrgID, connID)
	if err != nil {
		return nil, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	return sess.Overview(ctx)
}

// CancelBackend cancels a running statement, or ends the session when
// terminate is set.
func (a *App) CancelBackend(ctx context.Context, actor Actor, connID string, pid int64, terminate bool) error {
	if err := actor.require(PermServerAdmin); err != nil {
		return err
	}
	c, sess, err := a.open(ctx, actor.User.OrgID, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CancelBackend(ctx, pid, terminate); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "server.cancel_backend", c.ID, map[string]any{"connection": c.Name, "pid": pid, "terminate": terminate})
	return nil
}
