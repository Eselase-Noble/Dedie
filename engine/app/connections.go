package app

import (
	"context"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/secure"
	"dedie/engine/store"
)

var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

// ConnectionInput is what a user submits to create or change a connection.
// An empty password on update keeps the stored one.
type ConnectionInput struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	SSLMode     string `json:"sslmode"`
	SSLRootCert string `json:"ssl_root_cert"`
	ReadOnly    bool   `json:"read_only"`
}

func (in *ConnectionInput) validate() (connector.Driver, error) {
	in.Name, in.Host = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host)
	in.Database, in.Username = strings.TrimSpace(in.Database), strings.TrimSpace(in.Username)
	drv, err := connector.Get(in.Kind)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	if in.Name == "" || in.Host == "" || in.Database == "" || in.Username == "" {
		return nil, invalid("name, host, database and username are required")
	}
	if in.Port == 0 {
		in.Port = drv.DefaultPort()
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, invalid("port must be between 1 and 65535")
	}
	if in.SSLMode == "" {
		in.SSLMode = "require"
	}
	if !sslModes[in.SSLMode] {
		return nil, invalid("unknown SSL mode %q", in.SSLMode)
	}
	return drv, nil
}

// secretAAD binds an encrypted password to the record it belongs to.
func secretAAD(orgID, connID string) []byte { return []byte(orgID + "/" + connID) }

// config decrypts a stored connection into something a driver can open.
func (a *App) config(c store.Connection) (connector.Config, error) {
	pw, err := secure.Open(a.keyring, c.SecretEnc, secretAAD(c.OrgID, c.ID))
	if err != nil {
		return connector.Config{}, err
	}
	return connector.Config{Host: c.Host, Port: c.Port, Database: c.Database, Username: c.Username,
		Password: string(pw), SSLMode: c.SSLMode, SSLRootCert: c.SSLRootCert}, nil
}

// open loads a saved connection and opens a session on it.
func (a *App) open(ctx context.Context, orgID, connID string) (store.Connection, connector.Session, error) {
	c, err := a.Store.GetConnection(ctx, orgID, connID)
	if err != nil {
		return c, nil, wrap(err)
	}
	cfg, err := a.config(c)
	if err != nil {
		return c, nil, err
	}
	drv, err := connector.Get(c.Kind)
	if err != nil {
		return c, nil, err
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		return c, nil, &Error{Status: 502, Message: "could not connect: " + err.Error()}
	}
	return c, sess, nil
}

func (a *App) ListConnections(ctx context.Context, actor Actor) ([]store.Connection, error) {
	if err := actor.require(PermViewConnections); err != nil {
		return nil, err
	}
	return a.Store.ListConnections(ctx, actor.User.OrgID)
}

func (a *App) GetConnection(ctx context.Context, actor Actor, id string) (store.Connection, error) {
	if err := actor.require(PermViewConnections); err != nil {
		return store.Connection{}, err
	}
	c, err := a.Store.GetConnection(ctx, actor.User.OrgID, id)
	return c, wrap(err)
}

// SaveConnection creates a connection, or updates the one named by id.
func (a *App) SaveConnection(ctx context.Context, actor Actor, id string, in ConnectionInput) (store.Connection, error) {
	if err := actor.require(PermManageConnections); err != nil {
		return store.Connection{}, err
	}
	if _, err := in.validate(); err != nil {
		return store.Connection{}, err
	}
	c := store.Connection{ID: secure.NewID("con"), OrgID: actor.User.OrgID, CreatedBy: actor.User.Email}
	action := "connection.create"
	if id != "" {
		existing, err := a.Store.GetConnection(ctx, actor.User.OrgID, id)
		if err != nil {
			return c, wrap(err)
		}
		if existing.Kind != in.Kind {
			return c, invalid("the database kind of a connection cannot be changed")
		}
		c, action = existing, "connection.update"
	}
	c.Name, c.Kind, c.Host, c.Port = in.Name, in.Kind, in.Host, in.Port
	c.Database, c.Username, c.SSLMode, c.SSLRootCert, c.ReadOnly = in.Database, in.Username, in.SSLMode, in.SSLRootCert, in.ReadOnly
	if in.Password != "" || id == "" {
		enc, err := secure.Seal(a.keyring, []byte(in.Password), secretAAD(c.OrgID, c.ID))
		if err != nil {
			return c, err
		}
		c.SecretEnc = enc
	}
	if err := a.Store.SaveConnection(ctx, c); err != nil {
		return c, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, action, c.ID, map[string]any{"name": c.Name, "host": c.Host, "database": c.Database, "sslmode": c.SSLMode})
	return c, nil
}

func (a *App) DeleteConnection(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermManageConnections); err != nil {
		return err
	}
	c, err := a.Store.GetConnection(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if err := a.Store.DeleteConnection(ctx, actor.User.OrgID, c.ID); err != nil {
		return conflict("%s", err.Error())
	}
	a.audit(ctx, actor, "connection.delete", c.ID, map[string]string{"name": c.Name})
	return nil
}

// TestConnection opens a saved connection and reports what it reached.
func (a *App) TestConnection(ctx context.Context, actor Actor, id string) (connector.ServerInfo, error) {
	if err := actor.require(PermViewConnections); err != nil {
		return connector.ServerInfo{}, err
	}
	_, sess, err := a.open(ctx, actor.User.OrgID, id)
	if err != nil {
		return connector.ServerInfo{}, err
	}
	defer sess.Close(context.WithoutCancel(ctx))
	return sess.Info(ctx)
}

// TestInput tries connection settings before they are saved.
func (a *App) TestInput(ctx context.Context, actor Actor, in ConnectionInput) (connector.ServerInfo, error) {
	if err := actor.require(PermManageConnections); err != nil {
		return connector.ServerInfo{}, err
	}
	drv, err := in.validate()
	if err != nil {
		return connector.ServerInfo{}, err
	}
	sess, err := drv.Open(ctx, connector.Config{Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username,
		Password: in.Password, SSLMode: in.SSLMode, SSLRootCert: in.SSLRootCert})
	if err != nil {
		return connector.ServerInfo{}, &Error{Status: 502, Message: "could not connect: " + err.Error()}
	}
	defer sess.Close(context.WithoutCancel(ctx))
	return sess.Info(ctx)
}
