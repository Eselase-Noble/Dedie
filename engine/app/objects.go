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
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return c, nil, err
	}
	if c.ReadOnly {
		return c, nil, &Error{Status: 403, Message: "this connection is marked read-only"}
	}
	c, sess, err := a.open(ctx, actor, connID)
	return c, sess, err
}

// writableIn is writable for another database on the connection's server,
// reached with the same account. PostgreSQL ties a session to one
// database, so creating a schema or table elsewhere needs its own session.
func (a *App) writableIn(ctx context.Context, actor Actor, connID, database string) (store.Connection, connector.Session, error) {
	if database == "" {
		return a.writable(ctx, actor, connID)
	}
	if err := actor.require(PermQueryWrite); err != nil {
		return store.Connection{}, nil, err
	}
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return c, nil, err
	}
	if c.ReadOnly {
		return c, nil, &Error{Status: 403, Message: "this connection is marked read-only"}
	}
	cfg, err := a.config(c)
	if err != nil {
		return c, nil, err
	}
	cfg.Database = database
	drv, err := connector.Get(c.Kind)
	if err != nil {
		return c, nil, err
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		return c, nil, &Error{Status: 502, Message: "could not connect to " + database + ": " + err.Error()}
	}
	return c, sess, nil
}

// ServerObjects lists the databases and roles of a connection's server.
func (a *App) ServerObjects(ctx context.Context, actor Actor, connID string) (*connector.ServerObjects, error) {
	if err := actor.require(PermQueryRead); err != nil {
		return nil, err
	}
	_, sess, err := a.open(ctx, actor, connID)
	if err != nil {
		return nil, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	out, err := sess.ServerObjects(ctx)
	if err != nil {
		return nil, &Error{Status: 422, Message: err.Error()}
	}
	return out, nil
}

// SetOwner gives a database, schema or table to another role. database
// names the database that holds the schema or table when it is not the
// connection's own.
func (a *App) SetOwner(ctx context.Context, actor Actor, connID, database string, change connector.OwnerChange) error {
	if err := actor.require(PermServerAdmin); err != nil {
		return err
	}
	if change.Kind == connector.OwnDatabase {
		database = ""
	}
	c, sess, err := a.writableIn(ctx, actor, connID, database)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.SetOwner(ctx, change); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.set_owner", c.ID, map[string]string{"connection": c.Name, "database": database, "kind": change.Kind,
		"schema": change.Schema, "name": change.Name, "owner": change.Owner})
	return nil
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

// CreateSchema creates a schema in the connection's database, or in
// another database on the same server when one is named.
func (a *App) CreateSchema(ctx context.Context, actor Actor, connID, database, name string) error {
	c, sess, err := a.writableIn(ctx, actor, connID, database)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CreateSchema(ctx, name); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.create_schema", c.ID, map[string]string{"connection": c.Name, "database": database, "schema": name})
	return nil
}

func (a *App) CreateTable(ctx context.Context, actor Actor, connID, database string, def connector.TableDef) error {
	c, sess, err := a.writableIn(ctx, actor, connID, database)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	if err := sess.CreateTable(ctx, def); err != nil {
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "object.create_table", c.ID, map[string]any{"connection": c.Name, "database": database, "schema": def.Schema, "table": def.Name, "columns": len(def.Columns)})
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
	conns, err := a.visibleConnections(ctx, actor)
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

// ChangeRow inserts, updates or deletes one row from the data browser. The
// edit is audited with the row's key and the columns touched, not with the
// values written, which may be sensitive.
func (a *App) ChangeRow(ctx context.Context, actor Actor, connID string, change connector.RowChange) error {
	c, sess, err := a.writable(ctx, actor, connID)
	if err != nil {
		return err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	detail := map[string]any{"connection": c.Name, "schema": change.Schema, "table": change.Table, "action": change.Action,
		"key": change.Key, "columns": connector.Columns(change.Values)}
	if err := sess.ChangeRow(ctx, change); err != nil {
		detail["error"] = err.Error()
		a.audit(ctx, actor, "data.change_row", c.ID, detail)
		return &Error{Status: 422, Message: err.Error()}
	}
	a.audit(ctx, actor, "data.change_row", c.ID, detail)
	return nil
}
