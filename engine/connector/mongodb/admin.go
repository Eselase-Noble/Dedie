package mongodb

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dedie/engine/connector"
)

// Overview reports database sizes and the operations in progress. Seeing
// other sessions' operations needs the inprog privilege; without it only
// the sizes are returned.
func (s *session) Overview(ctx context.Context) (*connector.Overview, error) {
	out := &connector.Overview{Databases: []connector.DatabaseSize{}, Activity: []connector.Backend{},
		ReplicationSlots: []connector.ReplicationSlot{}, Settings: map[string]string{}}
	list, err := s.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, Friendly(err)
	}
	for _, d := range list.Databases {
		out.Databases = append(out.Databases, connector.DatabaseSize{Name: d.Name, SizeBytes: d.SizeOnDisk})
	}
	if version, err := Version(ctx, s.client); err == nil {
		out.Settings["version"] = version
	}
	if h, err := ServerHello(ctx, s.client); err == nil {
		out.Settings["replica set"] = h.SetName
		if h.SetName == "" {
			out.Settings["replica set"] = "none (standalone)"
		}
	}
	var ops struct {
		Inprog []struct {
			OpID    int64  `bson:"opid"`
			Op      string `bson:"op"`
			NS      string `bson:"ns"`
			Client  string `bson:"client"`
			AppName string `bson:"appName"`
			Secs    int64  `bson:"secs_running"`
			Active  bool   `bson:"active"`
			Command bson.Raw
		} `bson:"inprog"`
	}
	if s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}, {Key: "$all", Value: false}}).Decode(&ops) == nil {
		for _, op := range ops.Inprog {
			state := "idle"
			if op.Active {
				state = "active"
			}
			text := op.Op
			if len(op.Command) > 0 {
				if js, err := DocumentJSON(op.Command); err == nil {
					text = js
				}
			}
			if len(text) > 500 {
				text = text[:500] + "..."
			}
			out.Activity = append(out.Activity, connector.Backend{PID: op.OpID, Database: op.NS, Application: op.AppName,
				Client: op.Client, State: state, Seconds: op.Secs, Query: text})
		}
	}
	return out, nil
}

// CancelBackend stops a running operation. MongoDB has no separate way to
// end the whole session, so terminate does the same.
func (s *session) CancelBackend(ctx context.Context, pid int64, terminate bool) error {
	err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "killOp", Value: 1}, {Key: "op", Value: pid}}).Err()
	return Friendly(err)
}

func (s *session) CreateDatabase(ctx context.Context, name string) error {
	return errors.New("MongoDB creates a database when its first collection is created; add a connection for the new name and create a collection in it")
}

func (s *session) CreateSchema(ctx context.Context, name string) error {
	return errors.New("MongoDB has no schemas inside a database")
}

// CreateTable creates a collection. Columns are ignored: documents carry
// their own fields.
func (s *session) CreateTable(ctx context.Context, def connector.TableDef) error {
	if err := connector.CheckName("collection", def.Name); err != nil {
		return err
	}
	return Friendly(s.db.CreateCollection(ctx, def.Name))
}

func (s *session) ServerObjects(ctx context.Context) (*connector.ServerObjects, error) {
	out := &connector.ServerObjects{Databases: []connector.Database{}, Roles: []string{}}
	names, err := s.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, Friendly(err)
	}
	for _, n := range names {
		if n != "admin" && n != "local" && n != "config" {
			out.Databases = append(out.Databases, connector.Database{Name: n})
		}
	}
	return out, nil
}

func (s *session) SetOwner(ctx context.Context, c connector.OwnerChange) error {
	return errors.New("MongoDB collections have no owner; access is controlled with roles")
}

func (s *session) ChangeRow(ctx context.Context, c connector.RowChange) error {
	return errors.New("documents are edited with update commands in the query editor, not in the grid")
}
