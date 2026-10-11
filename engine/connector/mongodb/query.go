package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"dedie/engine/connector"
)

// readCommands are the database commands that cannot change data, and are
// therefore allowed without write access.
var readCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true, "listCollections": true,
	"listIndexes": true, "collStats": true, "dbStats": true, "explain": true, "ping": true,
	"hello": true, "buildInfo": true, "serverStatus": true, "listDatabases": true,
}

// cursorCommands answer with a cursor, which is read as rows.
var cursorCommands = map[string]bool{"find": true, "aggregate": true, "listCollections": true, "listIndexes": true}

// maxColumns bounds how many distinct fields become columns; documents in
// one collection need not share a shape.
const maxColumns = 300

// Query runs one database command written as a JSON document, the form
// MongoDB's own runCommand takes, for example:
//
//	{"find": "orders", "filter": {"status": "open"}, "sort": {"created": -1}, "limit": 50}
//	{"aggregate": "orders", "pipeline": [{"$group": {"_id": "$status", "n": {"$sum": 1}}}], "cursor": {}}
//
// Each document of the answer becomes a row and each top-level field a
// column. In read-only mode only commands that cannot write are accepted.
func (s *session) Query(ctx context.Context, text string, opts connector.QueryOptions) ([]connector.ResultSet, error) {
	if opts.MaxRows <= 0 {
		opts.MaxRows = 1000
	}
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = 60
	}
	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(strings.TrimSpace(text)), false, &cmd); err != nil {
		return nil, errors.New(`write a MongoDB command as JSON, for example {"find": "orders", "filter": {}, "limit": 50}: ` + err.Error())
	}
	if len(cmd) == 0 {
		return nil, errors.New("the command is empty")
	}
	name := cmd[0].Key
	if opts.ReadOnly {
		if !readCommands[name] {
			return nil, fmt.Errorf("%s can change data; allow writes to run it", name)
		}
		if name == "aggregate" && writesOutput(cmd) {
			return nil, errors.New("an aggregation with $out or $merge writes a collection; allow writes to run it")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds)*time.Second)
	defer cancel()

	if !cursorCommands[name] {
		raw, err := s.db.RunCommand(ctx, cmd).Raw()
		if err != nil {
			return nil, Friendly(err)
		}
		rs := rows([]bson.Raw{withoutClusterFields(raw)})
		rs.Command = name
		return []connector.ResultSet{rs}, nil
	}
	cur, err := s.db.RunCommandCursor(ctx, cmd)
	if err != nil {
		return nil, Friendly(err)
	}
	defer cur.Close(context.WithoutCancel(ctx))
	var docs []bson.Raw
	truncated := false
	for cur.Next(ctx) {
		if len(docs) >= opts.MaxRows {
			truncated = true
			break
		}
		// The cursor reuses its buffer, so each document is copied.
		docs = append(docs, append(bson.Raw(nil), cur.Current...))
	}
	if err := cur.Err(); err != nil {
		return nil, Friendly(err)
	}
	rs := rows(docs)
	rs.Truncated = truncated
	rs.Command = fmt.Sprintf("%s %d", name, len(docs))
	return []connector.ResultSet{rs}, nil
}

// withoutClusterFields drops the replication bookkeeping a replica set
// attaches to every answer, which says nothing about the command.
func withoutClusterFields(raw bson.Raw) bson.Raw {
	var doc bson.D
	if bson.Unmarshal(raw, &doc) != nil {
		return raw
	}
	kept := doc[:0]
	for _, e := range doc {
		switch e.Key {
		case "$clusterTime", "operationTime", "electionId", "opTime":
		default:
			kept = append(kept, e)
		}
	}
	if out, err := bson.Marshal(kept); err == nil {
		return out
	}
	return raw
}

// writesOutput reports whether an aggregate command has a stage that
// writes its result to a collection.
func writesOutput(cmd bson.D) bool {
	for _, e := range cmd {
		if e.Key != "pipeline" {
			continue
		}
		stages, _ := e.Value.(bson.A)
		for _, stage := range stages {
			if d, ok := stage.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
				return true
			}
		}
	}
	return false
}

// rows lays documents out as a table: one column per top-level field, in
// the order fields are first seen.
func rows(docs []bson.Raw) connector.ResultSet {
	rs := connector.ResultSet{Columns: []connector.Column{}, Rows: [][]*string{}}
	index := map[string]int{}
	for _, doc := range docs {
		elems, _ := doc.Elements()
		for _, e := range elems {
			key := e.Key()
			if _, ok := index[key]; !ok && len(rs.Columns) < maxColumns {
				index[key] = len(rs.Columns)
				rs.Columns = append(rs.Columns, connector.Column{Name: key})
			}
		}
	}
	for _, doc := range docs {
		row := make([]*string, len(rs.Columns))
		elems, _ := doc.Elements()
		for _, e := range elems {
			i, ok := index[e.Key()]
			if !ok {
				continue
			}
			v := e.Value()
			row[i] = Text(v)
			if rs.Columns[i].Type == "" && row[i] != nil {
				rs.Columns[i].Type = TypeName(v.Type)
			}
		}
		rs.Rows = append(rs.Rows, row)
	}
	return rs
}

func (s *session) Info(ctx context.Context) (connector.ServerInfo, error) {
	version, err := Version(ctx, s.client)
	if err != nil {
		return connector.ServerInfo{}, err
	}
	tlsConfig, _ := TLSConfig(s.cfg)
	info := connector.ServerInfo{Kind: Kind, Version: version, VersionNum: VersionNum(version), Database: s.cfg.Database,
		User: s.cfg.Username, Encrypted: tlsConfig != nil}
	// An account that may list every database is an administrator in all
	// but name; that is what "superuser" means to the interface.
	if _, err := s.client.ListDatabaseNames(ctx, bson.D{}); err == nil {
		var status struct {
			AuthInfo struct {
				Roles []struct {
					Role string `bson:"role"`
				} `bson:"authenticatedUserRoles"`
			} `bson:"authInfo"`
		}
		if s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&status) == nil {
			for _, r := range status.AuthInfo.Roles {
				if r.Role == "root" || strings.HasSuffix(r.Role, "AnyDatabase") {
					info.Superuser = true
				}
			}
		}
	}
	return info, nil
}

// Catalog lists the database's collections. MongoDB has no declared
// columns, so each collection's fields are taken from a sample of its
// documents and a field's type is the one seen first.
func (s *session) Catalog(ctx context.Context) (*connector.Catalog, error) {
	specs, err := s.db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, Friendly(err)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	schema := connector.Schema{Name: s.cfg.Database, Tables: []connector.Table{}, Views: []connector.Table{}, Functions: []string{}, Sequences: []string{}}
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		t := connector.Table{Name: spec.Name, Columns: []connector.TableColumn{}, Indexes: []string{}}
		coll := s.db.Collection(spec.Name)
		if spec.Type == "view" {
			schema.Views = append(schema.Views, t)
			continue
		}
		t.EstRows, _ = coll.EstimatedDocumentCount(ctx)
		var stats struct {
			Size int64 `bson:"storageSize"`
		}
		if s.db.RunCommand(ctx, bson.D{{Key: "collStats", Value: spec.Name}}).Decode(&stats) == nil {
			t.Size = stats.Size
		}
		t.Columns = sampleFields(ctx, coll)
		if idx, err := coll.Indexes().ListSpecifications(ctx); err == nil {
			for _, ix := range idx {
				keys, _ := bson.MarshalExtJSON(ix.KeysDocument, false, false)
				def := ix.Name + " " + string(keys)
				if ix.Unique != nil && *ix.Unique {
					def += " unique"
				}
				t.Indexes = append(t.Indexes, def)
			}
		}
		schema.Tables = append(schema.Tables, t)
	}
	return &connector.Catalog{Schemas: []connector.Schema{schema}}, nil
}

func sampleFields(ctx context.Context, coll *mongo.Collection) []connector.TableColumn {
	cols := []connector.TableColumn{}
	cur, err := coll.Find(ctx, bson.D{})
	if err != nil {
		return cols
	}
	defer cur.Close(context.WithoutCancel(ctx))
	index := map[string]int{}
	for n := 0; n < 100 && cur.Next(ctx); n++ {
		elems, _ := cur.Current.Elements()
		for _, e := range elems {
			key := e.Key()
			i, ok := index[key]
			if !ok {
				if len(cols) >= maxColumns {
					continue
				}
				i = len(cols)
				index[key] = i
				cols = append(cols, connector.TableColumn{Name: key, Nullable: key != "_id", PrimaryKey: key == "_id"})
			}
			if v := e.Value(); cols[i].Type == "" && v.Type != bson.TypeNull {
				cols[i].Type = TypeName(v.Type)
			}
		}
	}
	return cols
}
