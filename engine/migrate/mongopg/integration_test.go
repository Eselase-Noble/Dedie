package mongopg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// The integration test needs a MongoDB replica set member and a
// PostgreSQL server:
//
//	DEDIE_TEST_MONGO_SOURCE=mongodb://127.0.0.1:27117
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
//
// It creates and drops its own database on both. A single mongod started
// with --replSet that has not been initiated yet is initiated by the test.

func testConfig(t *testing.T, env string) connector.Config {
	raw := os.Getenv(env)
	if raw == "" {
		t.Skip(env + " not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	return connector.Config{Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		Username: u.User.Username(), Password: pw, SSLMode: "disable"}
}

type memReporter struct {
	mu      sync.Mutex
	t       *testing.T
	desired string
	phase   string
	state   State
	tables  map[string]map[string]any
}

func (m *memReporter) Event(level, msg string) { m.t.Logf("[%s] %s", level, msg) }
func (m *memReporter) SetPhase(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = p
}
func (m *memReporter) SaveState(s any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s.(State)
}
func (m *memReporter) ResetTables([]migrate.TableProgress) {}
func (m *memReporter) UpdateTable(_, table string, f map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tables[table] == nil {
		m.tables[table] = map[string]any{}
	}
	for k, v := range f {
		m.tables[table][k] = v
	}
}
func (m *memReporter) Desired() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.desired
}
func (m *memReporter) SetDesired(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desired = d
}
func (m *memReporter) snapshot() (string, State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase, m.state
}

func TestMigration(t *testing.T) {
	srcCfg := testConfig(t, "DEDIE_TEST_MONGO_SOURCE")
	dstCfg := testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	if h, _ := mongodb.ServerHello(ctx, client); h.SetName == "" {
		member := fmt.Sprintf("%s:%d", srcCfg.Host, srcCfg.Port)
		cfg := bson.D{{Key: "_id", Value: "rs0"}, {Key: "members", Value: bson.A{bson.D{{Key: "_id", Value: 0}, {Key: "host", Value: member}}}}}
		if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetInitiate", Value: cfg}}).Err(); err != nil {
			t.Logf("replSetInitiate: %v", err)
		}
		for i := 0; i < 60; i++ {
			if h, _ := mongodb.ServerHello(ctx, client); h.Primary {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	name := fmt.Sprintf("dedie_mongo_%d", time.Now().UnixNano()%1e9)
	srcCfg.Database = name
	db := client.Database(name)
	defer db.Drop(context.Background())

	// Documents of different shapes, keys of different types, and the
	// values that need care on the way into jsonb.
	oid := bson.NewObjectID()
	var users []any
	for i := 1; i <= 3000; i++ {
		users = append(users, bson.D{{Key: "_id", Value: int64(i)}, {Key: "name", Value: fmt.Sprintf("user %d ☃", i)},
			{Key: "tags", Value: bson.A{"a", "b"}}, {Key: "address", Value: bson.D{{Key: "city", Value: "Accra"}, {Key: "zip", Value: i}}},
			{Key: "joined", Value: time.Date(2024, 2, 29, 13, 14, 15, 0, time.UTC)}, {Key: "balance", Value: float64(i) * 1.25}})
	}
	if _, err := db.Collection("users").InsertMany(ctx, users); err != nil {
		t.Fatal(err)
	}
	dec, _ := bson.ParseDecimal128("12345678901234567890.123456789")
	mixed := []any{
		bson.D{{Key: "_id", Value: oid}, {Key: "kind", Value: "objectid key"}, {Key: "price", Value: dec}, {Key: "blob", Value: bson.Binary{Data: []byte{0, 1, 2, 255}}}},
		bson.D{{Key: "_id", Value: "text key"}, {Key: "null char", Value: "a\x00b"}, {Key: "big", Value: int64(9007199254740993)}},
		bson.D{{Key: "_id", Value: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: "x"}}}, {Key: "kind", Value: "compound key"}, {Key: "nothing", Value: nil}},
	}
	if _, err := db.Collection("mixed").InsertMany(ctx, mixed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("skipped").InsertOne(ctx, bson.D{{Key: "x", Value: 1}}); err != nil {
		t.Fatal(err)
	}

	admin, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	dstCfg.Database = name
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(context.Background())

	// Fields lifted into columns: plain and nested ones, a date, and one
	// whose type the documents do not have, which must come out NULL.
	cfg := migrate.Config{ExcludeTables: []string{"skipped"}, Columns: map[string][]migrate.ColumnSpec{"users": {
		{Field: "name", Type: "text"}, {Field: "balance", Type: "double precision"}, {Field: "joined", Type: "timestamptz"},
		{Field: "address.city", Type: "text"}, {Field: "address.zip", Name: "zip", Type: "bigint"}, {Field: "touched", Type: "bigint"},
		{Field: "name", Name: "name_as_number", Type: "bigint"},
	}}}
	suggested, err := Suggest(ctx, srcCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sp := range suggested["users"] {
		got[sp.Field] = sp.Type
	}
	if got["name"] != "text" || got["balance"] != "double precision" || got["joined"] != "timestamptz" || got["address"] != "" || got["tags"] != "" {
		t.Errorf("suggested columns for users: %v", got)
	}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		t.Logf("preflight %-5s %s: %s", c.Status, c.Name, c.Detail)
	}
	if !report.OK || report.Tables != 2 {
		t.Fatalf("preflight: ok=%v collections=%d", report.OK, report.Tables)
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	type result struct {
		out migrate.Outcome
		err error
	}
	start := func(st State) chan result {
		ch := make(chan result, 1)
		go func() {
			out, err := Run(ctx, "job1", cfg, st, srcCfg, dstCfg, rep)
			ch <- result{out, err}
		}()
		return ch
	}
	waitStreaming := func() {
		t.Helper()
		for i := 0; i < 400; i++ {
			if phase, _ := rep.snapshot(); phase == migrate.PhaseStreaming {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the job did not reach the streaming phase")
	}

	// Writes that run while the copy and the stream are going.
	stopWrites := make(chan struct{})
	var writes sync.WaitGroup
	writes.Add(1)
	go func() {
		defer writes.Done()
		coll := db.Collection("users")
		for i := 0; ; i++ {
			select {
			case <-stopWrites:
				return
			default:
			}
			id := int64(100000 + i)
			coll.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "name", Value: "live"}})
			coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: int64(1 + i%3000)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "touched", Value: i}}}})
			if i%3 == 0 {
				coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	done := start(State{})
	waitStreaming()
	time.Sleep(time.Second)

	// Pause, change the source while nothing is running, and resume.
	rep.SetDesired("pause")
	r := <-done
	if r.err != nil || r.out != migrate.Paused {
		t.Fatalf("pause: %v %v", r.out, r.err)
	}
	if _, err := db.Collection("late").InsertOne(ctx, bson.D{{Key: "_id", Value: "created while paused"}, {Key: "n", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("mixed").ReplaceOne(ctx, bson.D{{Key: "_id", Value: oid}}, bson.D{{Key: "kind", Value: "replaced"}}); err != nil {
		t.Fatal(err)
	}
	_, st := rep.snapshot()
	rep.SetDesired("run")
	done = start(st)
	waitStreaming()
	time.Sleep(time.Second)

	close(stopWrites)
	writes.Wait()
	rep.SetDesired("cutover")
	r = <-done
	if r.err != nil || r.out != migrate.Completed {
		t.Fatalf("cutover: %v %v", r.out, r.err)
	}

	// Every collection has the same keys on both sides.
	for _, coll := range []string{"users", "mixed", "late"} {
		want, _ := db.Collection(coll).CountDocuments(ctx, bson.D{})
		var got int64
		if err := dst.QueryRow(ctx, `SELECT count(*) FROM `+qname(coll)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want || want == 0 {
			t.Errorf("%s: %d rows in the target, %d documents in the source", coll, got, want)
		}
	}
	var skipped bool
	dst.QueryRow(ctx, `SELECT to_regclass('public.skipped') IS NOT NULL`).Scan(&skipped)
	if skipped {
		t.Error("the excluded collection was migrated")
	}
	if rep.tables["users"]["validation"] != "match" {
		t.Errorf("users validation: %v", rep.tables["users"])
	}

	// Contents survive: nested fields, updates applied by the stream, the
	// replaced document, and values that jsonb needs help with.
	var city, joined, kind, big, nul string
	var touched int
	if err := dst.QueryRow(ctx, `SELECT doc->'address'->>'city', doc->'joined'->>'$date' FROM users WHERE _id='7'`).Scan(&city, &joined); err != nil {
		t.Fatal(err)
	}
	if city != "Accra" || !strings.HasPrefix(joined, "2024-02-29T13:14:15") {
		t.Errorf("user 7: city %q joined %q", city, joined)
	}
	dst.QueryRow(ctx, `SELECT count(*) FROM users WHERE doc ? 'touched'`).Scan(&touched)
	if touched == 0 {
		t.Error("no update from the stream reached the target")
	}
	var srcTouched int64
	srcTouched, _ = db.Collection("users").CountDocuments(ctx, bson.D{{Key: "touched", Value: bson.D{{Key: "$exists", Value: true}}}})
	if int64(touched) != srcTouched {
		t.Errorf("%d updated documents in the target, %d in the source", touched, srcTouched)
	}
	dst.QueryRow(ctx, `SELECT doc->>'kind' FROM mixed WHERE _id=$1`, oid.Hex()).Scan(&kind)
	if kind != "replaced" {
		t.Errorf("replaced document: kind %q", kind)
	}
	dst.QueryRow(ctx, `SELECT doc->>'big', doc->>'null char' FROM mixed WHERE _id='text key'`).Scan(&big, &nul)
	if big != "9007199254740993" || nul != "ab" {
		t.Errorf("text key document: big %q, null char %q", big, nul)
	}
	// Lifted columns hold typed values, kept current by the stream.
	var userName, colCity string
	var balance float64
	var zip int64
	var joinedAt time.Time
	if err := dst.QueryRow(ctx, `SELECT name, balance, joined, address_city, zip FROM users WHERE _id='7'`).Scan(&userName, &balance, &joinedAt, &colCity, &zip); err != nil {
		t.Fatal(err)
	}
	if userName != "user 7 ☃" || balance != 8.75 || colCity != "Accra" || zip != 7 || !joinedAt.Equal(time.Date(2024, 2, 29, 13, 14, 15, 0, time.UTC)) {
		t.Errorf("columns of user 7: %q %v %q %d %s", userName, balance, colCity, zip, joinedAt)
	}
	var disagree, wrongType int
	dst.QueryRow(ctx, `SELECT count(*) FROM users WHERE touched IS DISTINCT FROM (doc->>'touched')::bigint`).Scan(&disagree)
	dst.QueryRow(ctx, `SELECT count(*) FROM users WHERE name_as_number IS NOT NULL`).Scan(&wrongType)
	if disagree != 0 || wrongType != 0 {
		t.Errorf("%d rows where the touched column and the document disagree; %d text values stored in a bigint column", disagree, wrongType)
	}
	if _, final := rep.snapshot(); final.Unconverted == 0 {
		t.Error("values of the wrong type for their column were not counted")
	}
	var compound int
	dst.QueryRow(ctx, `SELECT count(*) FROM mixed WHERE doc->>'kind'='compound key'`).Scan(&compound)
	if compound != 1 {
		t.Error("the document with a compound key is missing")
	}
	var checkpoint bool
	dst.QueryRow(ctx, `SELECT to_regclass('dedie.mongo_checkpoint') IS NOT NULL`).Scan(&checkpoint)
	if checkpoint {
		t.Error("the checkpoint table was left on the target")
	}
	_, final := rep.snapshot()
	t.Logf("applied %d changes in %d transactions, %d anomalies", final.RowsApplied, final.TxApplied, final.Anomalies)
}
