package app

import (
	"context"
	"sync"
	"time"

	"dedie/engine/connector"
	"dedie/engine/secure"
	"dedie/engine/store"
)

// writable opens a connection for an operation that changes the server. It
// requires write permission in Dedie and a connection not marked read-only;
// whether the database account has the privilege is left to the server.
func (a *App) writable(ctx context.Context, actor Actor, connID string) (store.Connection, connector.Session, error) {
	if err := actor.require(PermQueryWrite); err != nil {
		return store.Connection{}, nil, err
	}
	c, err := a.Store.GetConnection(ctx, actor.User.OrgID, connID)
	if err != nil {
		return c, nil, wrap(err)
	}
	if c.ReadOnly {
		return c, nil, &Error{Status: 403, Message: "this connection is marked read-only"}
	}
	c, sess, err := a.open(ctx, actor.User.OrgID, connID)
	return c, sess, err
}

// CreateDatabase creates a database on the connection's server. With
// addConnection it also saves a connection to the new database, using the
// same server and credentials, and returns it.
func (a *App) CreateDatabase(ctx context.Context, actor Actor, connID, name string, addConnection bool) (*store.Connection, error) {
	if addConnection {
		if err := actor.require(PermManageConnections); err != nil {
			return nil, err
		}
	}
	c, sess, err := a.writable(ctx, actor, connID)
	if err != nil {
		return nil, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CreateDatabase(ctx, name); err != nil {
		return nil, &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.create_database", c.ID, map[string]string{"connection": c.Name, "database": name})
	if !addConnection {
		return nil, nil
	}

	// The stored password is bound to its connection, so it is decrypted
	// and sealed again for the new record.
	cfg, err := a.config(c)
	if err != nil {
		return nil, err
	}
	created := c
	created.ID = secure.NewID("con")
	created.Name = c.Name + " / " + name
	created.Database = name
	created.CreatedBy = actor.User.Email
	if created.SecretEnc, err = secure.Seal(a.keyring, []byte(cfg.Password), secretAAD(created.OrgID, created.ID)); err != nil {
		return nil, err
	}
	if err := a.Store.SaveConnection(ctx, created); err != nil {
		return nil, invalid("the database was created, but a connection for it could not be saved: %s", err.Error())
	}
	a.audit(ctx, actor, "connection.create", created.ID, map[string]any{"name": created.Name, "host": created.Host, "database": created.Database, "sslmode": created.SSLMode})
	return &created, nil
}

func (a *App) CreateSchema(ctx context.Context, actor Actor, connID, name string) error {
	c, sess, err := a.writable(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CreateSchema(ctx, name); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.create_schema", c.ID, map[string]string{"connection": c.Name, "schema": name})
	return nil
}

func (a *App) CreateTable(ctx context.Context, actor Actor, connID string, def connector.TableDef) error {
	c, sess, err := a.writable(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CreateTable(ctx, def); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.create_table", c.ID, map[string]any{"connection": c.Name, "schema": def.Schema, "table": def.Name, "columns": len(def.Columns)})
	return nil
}

// ConnectionStatus is the result of trying to reach one connection.
type ConnectionStatus struct {
	Online    bool      `json:"online"`
	Version   string    `json:"version,omitempty"`
	Encrypted bool      `json:"encrypted"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ConnectionStatuses tries every connection of the organization at once and
// reports which servers answer. A server gets a few seconds; one that does
// not answer in that time is reported as offline, not waited for.
func (a *App) ConnectionStatuses(ctx context.Context, actor Actor) (map[string]ConnectionStatus, error) {
	if err := actor.require(PermViewConnections); err != nil {
		return nil, err
	}
	conns, err := a.Store.ListConnections(ctx, actor.User.OrgID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ConnectionStatus, len(conns))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c store.Connection) {
			defer wg.Done()
			status := a.probe(ctx, c)
			mu.Lock()
			out[c.ID] = status
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out, nil
}

func (a *App) probe(ctx context.Context, c store.Connection) ConnectionStatus {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	status := ConnectionStatus{CheckedAt: time.Now().UTC()}
	start := time.Now()
	fail := func(err error) ConnectionStatus {
		status.Error = err.Error()
		status.LatencyMS = time.Since(start).Milliseconds()
		return status
	}
	cfg, err := a.config(c)
	if err != nil {
		return fail(err)
	}
	drv, err := connector.Get(c.Kind)
	if err != nil {
		return fail(err)
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer sess.Close(context.WithoutCancel(ctx))
	info, err := sess.Info(ctx)
	if err != nil {
		return fail(err)
	}
	status.Online, status.Version, status.Encrypted = true, info.Version, info.Encrypted
	status.LatencyMS = time.Since(start).Milliseconds()
	return status
}
