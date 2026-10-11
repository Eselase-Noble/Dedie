package mongopg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

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

// TestMigrationOffline runs a one-time copy. The job must finish by itself,
// with no cutover requested and without opening a change stream.
func TestMigrationOffline(t *testing.T) {
	srcCfg := testConfig(t, "DEDIE_TEST_MONGO_SOURCE")
	dstCfg := testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	name := fmt.Sprintf("dedie_mongo_%d", time.Now().UnixNano()%1e9)
	srcCfg.Database = name
	db := client.Database(name)
	defer db.Drop(context.Background())

	var users []any
	for i := 1; i <= 3000; i++ {
		users = append(users, bson.D{{Key: "_id", Value: int64(i)}, {Key: "name", Value: fmt.Sprintf("user %d", i)},
			{Key: "address", Value: bson.D{{Key: "city", Value: "Accra"}, {Key: "zip", Value: i}}}})
	}
	if _, err := db.Collection("users").InsertMany(ctx, users); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("notes").InsertOne(ctx, bson.D{{Key: "_id", Value: "only"}, {Key: "body", Value: "one document"}}); err != nil {
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

	cfg := migrate.Config{Mode: migrate.ModeOffline, FenceSource: true,
		Columns: map[string][]migrate.ColumnSpec{"users": {{Field: "address.zip", Name: "zip", Type: "bigint"}}}}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, c := range report.Checks {
		t.Logf("preflight %-5s %s: %s", c.Status, c.Name, c.Detail)
		switch c.Name {
		case "Offline copy":
			noted = c.Status == "warn"
		case "Replica set", "Change stream", "Cutover":
			t.Errorf("offline preflight should not check %q", c.Name)
		}
	}
	if !report.OK || !noted || report.Tables != 2 {
		t.Fatalf("offline preflight: ok=%v, offline note=%v, collections=%d", report.OK, noted, report.Tables)
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "offline1", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("offline run: %v %v", out, err)
	}
	phase, st := rep.snapshot()
	if phase != migrate.PhaseDone || rep.Desired() != "run" {
		t.Errorf("finished in phase %q with the operator's instruction %q", phase, rep.Desired())
	}
	if st.StartT != 0 || st.TxApplied != 0 {
		t.Errorf("offline state holds a stream position (%d) or applied changes (%d)", st.StartT, st.TxApplied)
	}

	for _, coll := range []string{"users", "notes"} {
		want, _ := db.Collection(coll).CountDocuments(ctx, bson.D{})
		var got int64
		if err := dst.QueryRow(ctx, `SELECT count(*) FROM `+qname(coll)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want || want == 0 {
			t.Errorf("%s: %d rows in the target, %d documents in the source", coll, got, want)
		}
		if rep.tables[coll]["validation"] != "match" {
			t.Errorf("%s validation: %v", coll, rep.tables[coll])
		}
	}
	var city string
	var zip int64
	if err := dst.QueryRow(ctx, `SELECT doc->'address'->>'city', zip FROM users WHERE _id='7'`).Scan(&city, &zip); err != nil {
		t.Fatal(err)
	}
	if city != "Accra" || zip != 7 {
		t.Errorf("user 7: city %q, zip %d", city, zip)
	}
	var checkpoint bool
	dst.QueryRow(ctx, `SELECT to_regclass('dedie.mongo_checkpoint') IS NOT NULL`).Scan(&checkpoint)
	if checkpoint {
		t.Error("the checkpoint table was left on the target")
	}

	// A job resumed after its copy goes straight to validation, which must
	// now fail: nothing carries this document over.
	if _, err := db.Collection("notes").InsertOne(ctx, bson.D{{Key: "_id", Value: "late write"}}); err != nil {
		t.Fatal(err)
	}
	out, err = Run(ctx, "offline1", cfg, st, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("resumed offline job over a changed source: %v %v", out, err)
	}
	if phase, _ := rep.snapshot(); phase == migrate.PhaseStreaming {
		t.Error("offline job went to streaming")
	}
}

// existingTables prepares a source with three collections and a target on
// which two of their names are taken: one by a table that holds a row, the
// other by a view.
func existingTables(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, db *mongo.Database, dst *pgx.Conn) {
	t.Helper()
	srcCfg, dstCfg = testConfig(t, "DEDIE_TEST_MONGO_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Disconnect(context.Background()) })
	name := fmt.Sprintf("dedie_mongo_%d", time.Now().UnixNano()%1e9)
	srcCfg.Database = name
	db = client.Database(name)
	t.Cleanup(func() { db.Drop(context.Background()) })
	for coll, n := range map[string]int{"users": 3, "notes": 1, "orders": 2} {
		for i := 1; i <= n; i++ {
			if _, err := db.Collection(coll).InsertOne(ctx, bson.D{{Key: "_id", Value: int64(i)}, {Key: "of", Value: coll}}); err != nil {
				t.Fatal(err)
			}
		}
	}

	admin, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close(context.Background())
	})
	dstCfg.Database = name
	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })

	// A target that holds none of the names passes, and lists nothing.
	report, err := Preflight(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Existing objects"); c.Status != "ok" || !report.OK || len(report.Existing) != 0 {
		t.Fatalf("clean target: ok=%v, existing objects %s: %s, listed %v", report.OK, c.Status, c.Detail, report.Existing)
	}
	if c := findCheck(t, report, "Target is empty"); c.Status != "ok" {
		t.Fatalf("clean target: target is empty %s: %s", c.Status, c.Detail)
	}

	if _, err := dst.PgConn().Exec(ctx, `
		CREATE TABLE users (other int);
		INSERT INTO users VALUES (42);
		CREATE VIEW notes AS SELECT 1 AS one;
	`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	return srcCfg, dstCfg, db, dst
}

func findCheck(t *testing.T, r *migrate.Report, name string) migrate.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("preflight has no %q check", name)
	return migrate.Check{}
}

// existingList is the preflight's list of what the target already has.
func existingList(r *migrate.Report) string {
	var out []string
	for _, e := range r.Existing {
		out = append(out, e.Kind+" "+e.Name)
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

// wantExisting is what the preflight must list on the target existingTables
// prepares.
const wantExisting = "table public.users; view public.notes"

// definitions describes the relations of the target's schema, the columns
// of the table that was there, what it holds, and a checkpoint if a job
// left one.
func definitions(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var s string
	if err := dst.QueryRow(ctx, `SELECT concat_ws(' | ',
		(SELECT string_agg(relname || ':' || relkind::text, ',' ORDER BY relname COLLATE "C") FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind <> 'i'),
		(SELECT string_agg(viewname || ':' || md5(definition), ',') FROM pg_views WHERE schemaname = 'public'),
		(SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'users'::regclass AND attnum > 0),
		(SELECT string_agg(u::text, ',') FROM users u),
		(SELECT to_regclass('dedie.mongo_checkpoint')::text))`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMigrationExistingObjects is the default policy: a table that is
// already on the target stops the job, which used to empty it.
func TestMigrationExistingObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := existingTables(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline}
	before := definitions(t, ctx, dst)

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Status == "fail" {
			t.Logf("preflight %-5s %s: %s", c.Status, c.Name, c.Detail)
		}
	}
	if report.OK {
		t.Error("preflight passed over a target that has the collections' names taken")
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	if c := findCheck(t, report, "Target is empty"); c.Status != "fail" || !strings.Contains(c.Detail, "these tables already exist in schema public on the target: users") {
		t.Errorf("target is empty: %s: %s", c.Status, c.Detail)
	}
	if c := findCheck(t, report, "Existing objects"); c.Status != "fail" || !strings.Contains(c.Detail, "tables: notes (the name of a view)") ||
		!strings.Contains(c.Detail, "never replaces or merges") {
		t.Errorf("existing objects: %s: %s", c.Status, c.Detail)
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "exist1", cfg, State{}, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") ||
		!strings.Contains(err.Error(), "Existing objects") || !strings.Contains(err.Error(), "Target is empty") {
		t.Fatalf("run over existing tables: %v %v", out, err)
	}
	// The table still holds its row, and nothing was created beside it.
	if after := definitions(t, ctx, dst); after != before {
		t.Errorf("the refused job changed the target:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMigrationExistingSkip keeps the tables the target has, while the
// source stays in use: their collections are neither copied nor followed,
// and the remaining one arrives with the changes made to it meanwhile.
func TestMigrationExistingSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, db, dst := existingTables(t, ctx)
	cfg := migrate.Config{OnExisting: migrate.ExistingSkip}
	before := definitions(t, ctx, dst)

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Status == "fail" {
			t.Errorf("preflight %s: %s", c.Name, c.Detail)
		}
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	for _, name := range []string{"Target is empty", "Existing objects"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-5s %s: %s", c.Status, c.Name, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "kept exactly as they are") || !strings.Contains(c.Detail, "left out of the migration") {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	type result struct {
		out migrate.Outcome
		err error
	}
	start := func(st State) chan result {
		ch := make(chan result, 1)
		go func() {
			out, err := Run(ctx, "skip1", cfg, st, srcCfg, dstCfg, rep)
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
	done := start(State{})
	waitStreaming()
	// Changes to a kept collection must go nowhere; the others are followed.
	for _, coll := range []string{"users", "notes", "orders"} {
		if _, err := db.Collection(coll).InsertOne(ctx, bson.D{{Key: "_id", Value: int64(100)}, {Key: "of", Value: "the stream"}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Second)

	// Pause and resume: the job must still know what it left out, now that
	// its own table exists as well.
	rep.SetDesired("pause")
	if r := <-done; r.err != nil || r.out != migrate.Paused {
		t.Fatalf("pause: %v %v", r.out, r.err)
	}
	if _, err := db.Collection("users").InsertOne(ctx, bson.D{{Key: "_id", Value: int64(101)}}); err != nil {
		t.Fatal(err)
	}
	_, st := rep.snapshot()
	sort.Strings(st.SkippedTables)
	if strings.Join(st.SkippedTables, ",") != "notes,users" || len(st.DropFirst) != 0 || strings.Join(st.Tables, ",") != "orders" {
		t.Errorf("remembered: skipped %v, drop %v, tables %v", st.SkippedTables, st.DropFirst, st.Tables)
	}
	rep.SetDesired("run")
	done = start(st)
	waitStreaming()
	rep.SetDesired("cutover")
	if r := <-done; r.err != nil || r.out != migrate.Completed {
		t.Fatalf("cutover: %v %v", r.out, r.err)
	}

	want := strings.Replace(before, "notes:v,users:r", "notes:v,orders:r,users:r", 1)
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after keeping what was there:\nwant %s\ngot  %s", want, after)
	}
	var orders string
	if err := dst.QueryRow(ctx, `SELECT string_agg(_id, ',' ORDER BY _id::int) FROM orders`).Scan(&orders); err != nil || orders != "1,2,100" {
		t.Errorf("the collection whose name was free holds %q: %v", orders, err)
	}
	if v, _ := rep.tables["orders"]["validation"].(string); len(rep.tables) != 1 || !strings.Contains(v, "match") || v == "mismatch" {
		t.Errorf("collections copied and validated: %v", rep.tables)
	}
}

// TestMigrationExistingReplace drops what the target has under the
// collections' names and creates their tables in its place.
func TestMigrationExistingReplace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := existingTables(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Error("preflight failed although the job replaces what is there")
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	for name, object := range map[string]string{"Target is empty": "users", "Existing objects": "notes (the name of a view)"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-5s %s: %s", c.Status, c.Name, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "DROPPED") || !strings.Contains(c.Detail, object) {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	var dropped bool
	rep := &eventReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(msg string) {
		dropped = dropped || strings.Contains(msg, "dropped 1 existing table(s) and 1 other object(s) on the target, as chosen")
	}}
	out, err := Run(ctx, "replace1", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	if !dropped {
		t.Error("the job did not report what it dropped")
	}
	const want = "notes:r,orders:r,users:r | _id,doc | " +
		`(1,"{""of"": ""users"", ""_id"": 1}"),(2,"{""of"": ""users"", ""_id"": 2}"),(3,"{""of"": ""users"", ""_id"": 3}")`
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after replacing:\nwant %s\ngot  %s", want, after)
	}
	var notes int
	if err := dst.QueryRow(ctx, `SELECT count(*) FROM notes WHERE doc->>'of' = 'notes'`).Scan(&notes); err != nil || notes != 1 {
		t.Errorf("the table that replaced the view holds %d document(s): %v", notes, err)
	}
	for name, tbl := range rep.tables {
		if v, _ := tbl["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
			t.Errorf("collection %s validation = %v", name, tbl["validation"])
		}
	}
}

// eventReporter lets a test see the events of a job.
type eventReporter struct {
	*memReporter
	on func(msg string)
}

func (e *eventReporter) Event(level, msg string) {
	e.on(msg)
	e.memReporter.Event(level, msg)
}

// previewLine writes one statement of a preview for comparison.
func previewLine(s migrate.Statement) string { return s.Phase + " | " + s.Kind + " | " + s.Name }

// findStatement returns the previewed statement of that kind and name.
func findStatement(t *testing.T, list []migrate.Statement, kind, name string) migrate.Statement {
	t.Helper()
	for _, s := range list {
		if s.Kind == kind && s.Name == name {
			return s
		}
	}
	t.Fatalf("the preview has no %s %s", kind, name)
	return migrate.Statement{}
}

// checkPhaseOrder fails unless the statements come phase by phase, in the
// order a job moves through the phases, each complete and unterminated.
func checkPhaseOrder(t *testing.T, list []migrate.Statement) {
	t.Helper()
	order := map[string]int{migrate.PhaseSchema: 1, migrate.PhaseIndexing: 2, migrate.PhaseCutover: 3}
	last := 0
	for _, s := range list {
		n := order[s.Phase]
		if n == 0 || n < last {
			t.Errorf("%s: out of phase order", previewLine(s))
		}
		last = n
		if s.Kind == "" || s.Name == "" || strings.TrimSpace(s.SQL) == "" || strings.HasSuffix(strings.TrimSpace(s.SQL), ";") {
			t.Errorf("%s: incomplete or terminated statement %q", previewLine(s), s.SQL)
		}
	}
}

// TestMigrationPreview reads the statements of a migration before it is
// started. A preview must be the real script, so the test runs it, as it
// is, against the target; and it must be only a preview, so both databases
// are compared with what they were before.
func TestMigrationPreview(t *testing.T) {
	var _ migrate.Previewer = pipeline{}
	srcCfg := testConfig(t, "DEDIE_TEST_MONGO_SOURCE")
	dstCfg := testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	name := fmt.Sprintf("dedie_mongo_%d", time.Now().UnixNano()%1e9)
	srcCfg.Database = name
	db := client.Database(name)
	defer db.Drop(context.Background())
	if _, err := db.Collection("users").InsertOne(ctx, bson.D{{Key: "_id", Value: int64(1)}, {Key: "name", Value: "user 1"},
		{Key: "address", Value: bson.D{{Key: "city", Value: "Accra"}, {Key: "zip", Value: 1}}}}); err != nil {
		t.Fatal(err)
	}
	for _, coll := range []string{"notes", "scratch"} {
		if _, err := db.Collection(coll).InsertOne(ctx, bson.D{{Key: "_id", Value: "only"}, {Key: "body", Value: "one document"}}); err != nil {
			t.Fatal(err)
		}
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

	// Everything a job leaves behind: the collections of the source, and
	// on the target the relations of every schema that is not the server's
	// own, the checkpoint schema among them.
	state := func() (source, target string) {
		t.Helper()
		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if err := dst.QueryRow(ctx, `SELECT concat_ws(' | ',
			(SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'),
			coalesce((SELECT string_agg(c.relname || ':' || c.relkind::text, ',' ORDER BY c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'), ''),
			coalesce((SELECT string_agg(a.attrelid::regclass::text || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod), ',' ORDER BY a.attrelid::regclass::text, a.attnum)
				FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0), ''))`).Scan(&target); err != nil {
			t.Fatal(err)
		}
		return strings.Join(names, ","), target
	}
	srcBefore, dstBefore := state()
	if dstBefore != "public |  | " {
		t.Fatalf("the target is not empty: %s", dstBefore)
	}
	unchanged := func(when string) {
		t.Helper()
		source, target := state()
		if source != srcBefore {
			t.Errorf("%s changed the source:\nbefore %s\nafter  %s", when, srcBefore, source)
		}
		if target != dstBefore {
			t.Errorf("%s changed the target:\nbefore %s\nafter  %s", when, dstBefore, target)
		}
	}

	// A table for each collection in scope, with the columns its fields are
	// lifted into. Tables are all a collection becomes, so there is nothing
	// for the later phases.
	cfg := migrate.Config{ExcludeTables: []string{name + ".scratch"},
		Columns: map[string][]migrate.ColumnSpec{"users": {{Field: "address.zip", Name: "zip", Type: "bigint"}, {Field: "name", Type: "text"}}}}
	list, err := pipeline{}.Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	unchanged("the preview")
	want := []migrate.Statement{
		{Phase: migrate.PhaseSchema, Kind: "table", Name: "notes", SQL: `CREATE TABLE IF NOT EXISTS "public"."notes" (_id text PRIMARY KEY, doc jsonb NOT NULL)`},
		{Phase: migrate.PhaseSchema, Kind: "table", Name: "users", SQL: `CREATE TABLE IF NOT EXISTS "public"."users" (_id text PRIMARY KEY, doc jsonb NOT NULL, "zip" bigint, "name" text)`},
	}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Errorf("preview:\n got %v\nwant %v", list, want)
	}
	checkPhaseOrder(t, list)

	// A job for named collections previews those only.
	scoped, err := Preview(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline, Tables: []string{"notes"}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(scoped) != fmt.Sprint(want[:1]) {
		t.Errorf("collection scope previews %v", scoped)
	}

	// What the target already has. From here on the previews depend on it.
	if _, err := dst.Exec(ctx, `CREATE TABLE notes (other int); INSERT INTO notes VALUES (42)`); err != nil {
		t.Fatal(err)
	}
	_, dstBefore = state()

	// Under the default policy the job would refuse to start; its preview
	// still shows every table as it would be created.
	again, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(want) {
		t.Errorf("the default policy previews %v", again)
	}

	// Kept: the collection whose table is there is left out.
	cfg.OnExisting = migrate.ExistingSkip
	skip, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(skip) != fmt.Sprint(want[1:]) {
		t.Errorf("skip previews %v", skip)
	}

	// Replaced: the drop comes first, then the whole migration.
	cfg.OnExisting = migrate.ExistingReplace
	replace, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, replace)
	drop := migrate.Statement{Phase: migrate.PhaseSchema, Kind: "drop", Name: "public.notes", SQL: `DROP TABLE IF EXISTS public.notes CASCADE`}
	if fmt.Sprint(replace) != fmt.Sprint(append([]migrate.Statement{drop}, want...)) {
		t.Errorf("replace previews %v", replace)
	}
	unchanged("the previews")

	// The preview is the script: every statement runs, in order, as it is.
	for _, s := range replace {
		if _, err := dst.PgConn().Exec(ctx, s.SQL).ReadAll(); err != nil {
			t.Fatalf("%s: %v\n%s", previewLine(s), err, s.SQL)
		}
	}
	const built = "public | notes:r,notes_pkey:i,users:r,users_pkey:i | notes._id text,notes.doc jsonb,users._id text,users.doc jsonb,users.zip bigint,users.name text"
	if _, got := state(); got != built {
		t.Errorf("the script built:\n got %s\nwant %s", got, built)
	}
}
