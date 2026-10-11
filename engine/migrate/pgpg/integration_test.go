package pgpg

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

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// The integration test needs two PostgreSQL servers, the source with
// wal_level = logical. Point it at their maintenance databases:
//
//	DEDIE_TEST_SOURCE=postgres://postgres@127.0.0.1:54316/postgres
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
//
// It creates and drops its own databases on both.

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
func (m *memReporter) ResetTables([]TableProgress) {}
func (m *memReporter) UpdateTable(schema, table string, f map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tables[schema+"."+table] == nil {
		m.tables[schema+"."+table] = map[string]any{}
	}
	for k, v := range f {
		m.tables[schema+"."+table][k] = v
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

const fixture = `
CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');
CREATE DOMAIN email AS text CHECK (VALUE LIKE '%@%');
CREATE SCHEMA billing;
CREATE TABLE customers (
	id serial PRIMARY KEY, name text NOT NULL, contact email, feeling mood DEFAULT 'ok',
	profile jsonb, avatar bytea, tags text[], balance numeric(12,2) DEFAULT 0,
	created_at timestamptz DEFAULT now(), bio text,
	name_upper text GENERATED ALWAYS AS (upper(name)) STORED);
CREATE INDEX customers_name_idx ON customers (lower(name));
CREATE TABLE billing.invoices (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	customer_id int NOT NULL REFERENCES public.customers(id) ON UPDATE CASCADE ON DELETE CASCADE,
	amount numeric(12,2) NOT NULL CHECK (amount >= 0), ratio double precision, issued date DEFAULT current_date);
CREATE TABLE events (kind text, payload text);
ALTER TABLE events REPLICA IDENTITY FULL;
CREATE TABLE change_log (id serial PRIMARY KEY, note text);
CREATE TABLE scratch (v int);
CREATE FUNCTION log_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO public.change_log (note) VALUES (TG_OP || ' ' || NEW.id); RETURN NEW; END $$;
CREATE TRIGGER customers_log AFTER INSERT ON customers FOR EACH ROW EXECUTE FUNCTION log_change();
CREATE VIEW customer_totals AS
	SELECT c.id, c.name, coalesce(sum(i.amount), 0) AS total FROM customers c LEFT JOIN billing.invoices i ON i.customer_id = c.id GROUP BY c.id;
INSERT INTO customers (name, contact, profile, avatar, tags, bio)
	SELECT 'user ' || g, 'u' || g || '@example.com', jsonb_build_object('n', g), decode(md5(g::text), 'hex'), ARRAY['a', 'b''c'], repeat(md5(g::text), 200)
	FROM generate_series(1, 5000) g;
INSERT INTO billing.invoices (customer_id, amount, ratio) SELECT (g % 5000) + 1, g * 1.5, g / 7.0 FROM generate_series(1, 20000) g;
INSERT INTO events SELECT 'k' || (g % 10), 'payload ' || g FROM generate_series(1, 1000) g;
`

func TestMigration(t *testing.T) {
	srcAdmin, dstAdmin := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_test_%d", time.Now().UnixNano())
	srcCfg, dstCfg := srcAdmin, dstAdmin
	srcCfg.Database, dstCfg.Database = db, db
	for _, admin := range []connector.Config{srcAdmin, dstAdmin} {
		c, err := postgres.Connect(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
		admin := admin
		t.Cleanup(func() {
			bg := context.Background()
			if c, err := postgres.Connect(bg, admin); err == nil {
				c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
				c.Close(bg)
			}
		})
	}

	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	if _, err := src.PgConn().Exec(ctx, fixture).ReadAll(); err != nil {
		t.Fatal(err)
	}

	// Table-level scope: only the named tables, no views, and a clear
	// report of names that do not exist.
	scoped, err := BuildPlan(ctx, src, Config{Tables: []string{"billing.invoices", "customers", "public.nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Tables) != 2 || len(scoped.Missing) != 1 || scoped.Missing[0] != "public.nope" {
		t.Fatalf("table scope: %d tables, missing %v", len(scoped.Tables), scoped.Missing)
	}
	for _, s := range scoped.Post {
		if s.Kind == "view" {
			t.Errorf("table scope should not include view %s", s.Name)
		}
	}
	if schemaScoped, err := BuildPlan(ctx, src, Config{Schemas: []string{"billing"}}); err != nil || len(schemaScoped.Tables) != 1 {
		t.Fatalf("schema scope: %v, %d tables", err, len(schemaScoped.Tables))
	}
	// BuildPlan narrows the session's search_path; use a fresh session below.
	src.Close(ctx)
	if src, err = postgres.Connect(ctx, srcCfg); err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)

	cfg := Config{ExcludeTables: []string{"public.scratch"}, FenceSource: true, CutoverTimeoutSeconds: 60}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		t.Logf("preflight %-28s %-4s %s", c.Name, c.Status, c.Detail)
	}
	if !report.OK {
		t.Fatal("preflight failed")
	}

	// Writes that land while the snapshot and stream are running.
	workload := func(round int) {
		w, err := postgres.Connect(ctx, srcCfg)
		if err != nil {
			t.Error(err)
			return
		}
		defer w.Close(ctx)
		for i := 0; i < 200; i++ {
			n := round*1000 + i
			stmts := []string{
				fmt.Sprintf(`INSERT INTO customers (name, contact, bio) VALUES ('live %d', 'l%d@example.com', repeat('x', 5000))`, n, n),
				fmt.Sprintf(`UPDATE customers SET balance = balance + 1, feeling = 'happy' WHERE id = %d`, i+1),
				fmt.Sprintf(`INSERT INTO billing.invoices (customer_id, amount, ratio) VALUES (%d, 9.99, 0.1)`, i+1),
				fmt.Sprintf(`DELETE FROM billing.invoices WHERE id = %d`, n%20000+1),
				fmt.Sprintf(`UPDATE events SET payload = 'changed %d' WHERE payload = 'payload %d'`, n, i+1+round*200),
				fmt.Sprintf(`UPDATE customers SET id = id + 100000 WHERE id = %d`, 4000+i+round*200),
				fmt.Sprintf(`DELETE FROM customers WHERE id = %d`, 3000+i+round*200),
			}
			for _, s := range stmts {
				if _, err := w.Exec(ctx, s); err != nil {
					t.Errorf("workload: %v", err)
					return
				}
			}
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	run := func(st State) (Outcome, error) { return Run(ctx, "mig_test1", cfg, st, srcCfg, dstCfg, rep) }
	waitFor := func(what string, cond func(phase string, st State) bool) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if cond(rep.snapshot()) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() { o, e := run(State{}); done <- result{o, e} }()
	go workload(0)
	waitFor("streaming", func(p string, st State) bool { return p == PhaseStreaming })

	// Pause and resume: the stream must pick up exactly where it stopped.
	// The stream is only waited for by phase: on a shared test server the
	// first workload can finish before streaming begins.
	rep.SetDesired("pause")
	if r := <-done; r.err != nil || r.out != Paused {
		t.Fatalf("pause: %v %v", r.out, r.err)
	}
	workload(1)
	_, saved := rep.snapshot()
	rep.SetDesired("run")
	go func() { o, e := run(saved); done <- result{o, e} }()
	workload(2)
	if _, err := src.Exec(ctx, `TRUNCATE events`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(ctx, `INSERT INTO events VALUES ('after', 'truncate')`); err != nil {
		t.Fatal(err)
	}

	rep.SetDesired("cutover")
	select {
	case r := <-done:
		if r.err != nil || r.out != Completed {
			t.Fatalf("cutover: %v %v", r.out, r.err)
		}
	case <-ctx.Done():
		t.Fatal("cutover did not finish")
	}

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)
	check := func(sql string) {
		t.Helper()
		var a, b string
		if err := src.QueryRow(ctx, sql).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", sql, err)
		}
		if err := dst.QueryRow(ctx, sql).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", sql, err)
		}
		if a != b {
			t.Errorf("%s: source %s, target %s", sql, a, b)
		} else {
			t.Logf("ok  %-70s %s", sql, a)
		}
	}
	check(`SELECT count(*)::text FROM customers`)
	check(`SELECT md5(string_agg(c::text, '' ORDER BY id)) FROM customers c`)
	check(`SELECT md5(string_agg(i::text, '' ORDER BY id)) FROM billing.invoices i`)
	check(`SELECT md5(coalesce(string_agg(e::text, '' ORDER BY kind, payload), '')) FROM events e`)
	// The trigger must not have fired again on the target for replicated rows.
	check(`SELECT count(*)::text FROM change_log`)
	check(`SELECT last_value::text FROM customers_id_seq`)
	check(`SELECT last_value::text FROM billing.invoices_id_seq`)
	check(`SELECT count(*)::text FROM pg_trigger WHERE NOT tgisinternal`)
	check(`SELECT count(*)::text FROM pg_indexes WHERE schemaname IN ('public', 'billing') AND tablename <> 'scratch'`)
	check(`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f'`)
	check(`SELECT sum(total)::text FROM customer_totals`)

	// New rows on the target must get fresh ids from the synced sequences.
	if _, err := dst.Exec(ctx, `INSERT INTO customers (name) VALUES ('post cutover')`); err != nil {
		t.Errorf("insert on target after cutover: %v", err)
	}
	if _, err := dst.Exec(ctx, `INSERT INTO billing.invoices (customer_id, amount) VALUES (1, 1)`); err != nil {
		t.Errorf("identity insert on target after cutover: %v", err)
	}

	var slots, pubs int
	src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'dedie_mig_test1'`).Scan(&slots)
	src.QueryRow(ctx, `SELECT count(*) FROM pg_publication`).Scan(&pubs)
	if slots != 0 || pubs != 0 {
		t.Errorf("left behind %d slot(s) and %d publication(s) on the source", slots, pubs)
	}
	var metaLeft bool
	dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'dedie')`).Scan(&metaLeft)
	if metaLeft {
		t.Error("checkpoint schema left on the target")
	}

	// The fence must hold for ordinary new sessions.
	w, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	if _, err := w.Exec(ctx, `INSERT INTO change_log (note) VALUES ('late write')`); err == nil {
		t.Error("source accepted a write after a fenced cutover")
	}
	for name, tbl := range rep.tables {
		if tbl["validation"] != "match" && tbl["validation"] != "count match" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}
	_, st := rep.snapshot()
	t.Logf("applied %d transactions, %d row changes, %d anomalies", st.TxApplied, st.RowsApplied, st.Anomalies)
	if st.Anomalies != 0 {
		t.Errorf("%d apply anomalies", st.Anomalies)
	}
}

// TestMigrationOffline runs a one-time copy. The job must finish by itself,
// with no cutover requested, and leave nothing behind on the source.
func TestMigrationOffline(t *testing.T) {
	srcAdmin, dstAdmin := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_test_%d", time.Now().UnixNano())
	srcCfg, dstCfg := srcAdmin, dstAdmin
	srcCfg.Database, dstCfg.Database = db, db
	for _, admin := range []connector.Config{srcAdmin, dstAdmin} {
		c, err := postgres.Connect(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
		admin := admin
		t.Cleanup(func() {
			bg := context.Background()
			if c, err := postgres.Connect(bg, admin); err == nil {
				c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
				c.Close(bg)
			}
		})
	}
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	// scratch has no key at all: a stream could not follow it, a copy can.
	if _, err := src.PgConn().Exec(ctx, fixture+`INSERT INTO scratch SELECT g FROM generate_series(1, 100) g;`).ReadAll(); err != nil {
		t.Fatal(err)
	}

	// Fencing is asked for and must be ignored: an offline job has no cutover.
	cfg := Config{Mode: migrate.ModeOffline, FenceSource: true}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, c := range report.Checks {
		t.Logf("preflight %-28s %-4s %s", c.Name, c.Status, c.Detail)
		switch c.Name {
		case "Offline copy":
			noted = c.Status == "warn"
		case "Source wal_level", "Replication privilege", "Replication capacity", "Tables without a key":
			t.Errorf("offline preflight should not check %q", c.Name)
		}
	}
	if !report.OK || !noted {
		t.Fatalf("offline preflight: ok=%v, offline note=%v", report.OK, noted)
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_offline1", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != Completed {
		t.Fatalf("offline run: %v %v", out, err)
	}
	phase, st := rep.snapshot()
	if phase != PhaseDone || rep.Desired() != "run" {
		t.Errorf("finished in phase %q with the operator's instruction %q", phase, rep.Desired())
	}
	if st.Slot != "" || st.Fenced {
		t.Errorf("offline state names slot %q, fenced %v", st.Slot, st.Fenced)
	}

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)
	check := func(sql string) {
		t.Helper()
		var a, b string
		if err := src.QueryRow(ctx, sql).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", sql, err)
		}
		if err := dst.QueryRow(ctx, sql).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", sql, err)
		}
		if a != b {
			t.Errorf("%s: source %s, target %s", sql, a, b)
		}
	}
	check(`SELECT md5(string_agg(c::text, '' ORDER BY id)) FROM customers c`)
	check(`SELECT md5(string_agg(i::text, '' ORDER BY id)) FROM billing.invoices i`)
	check(`SELECT md5(string_agg(e::text, '' ORDER BY kind, payload)) FROM events e`)
	check(`SELECT sum(v)::text FROM scratch`)
	check(`SELECT last_value::text FROM customers_id_seq`)
	check(`SELECT count(*)::text FROM pg_indexes WHERE schemaname IN ('public', 'billing')`)
	check(`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f'`)
	check(`SELECT sum(total)::text FROM customer_totals`)
	for name, tbl := range rep.tables {
		if tbl["validation"] != "match" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}

	var slots, pubs int
	src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE database = current_database()`).Scan(&slots)
	src.QueryRow(ctx, `SELECT count(*) FROM pg_publication`).Scan(&pubs)
	if slots != 0 || pubs != 0 {
		t.Errorf("left behind %d slot(s) and %d publication(s) on the source", slots, pubs)
	}
	var metaLeft bool
	dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'dedie')`).Scan(&metaLeft)
	if metaLeft {
		t.Error("checkpoint schema left on the target")
	}

	// The source stays writable. A job resumed after its copy goes straight
	// to validation, which must now fail: nothing carries this row over.
	if _, err := src.Exec(ctx, `INSERT INTO change_log (note) VALUES ('late write')`); err != nil {
		t.Fatalf("source refused a write after an offline job: %v", err)
	}
	out, err = Run(ctx, "mig_offline1", cfg, st, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("resumed offline job over a changed source: %v %v", out, err)
	}
	if phase, _ := rep.snapshot(); phase == PhaseStreaming {
		t.Error("offline job went to streaming")
	}
}

// existingObjects prepares a source and a target on which every kind of
// object the plan creates already exists under a name the migration wants.
// None is what the source has under that name. Two of them hold a name the
// migration wants for an index: one a secondary index, the other the index
// behind a primary key.
func existingObjects(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, src, dst *pgx.Conn) {
	t.Helper()
	srcAdmin, dstAdmin := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	db := fmt.Sprintf("dedie_test_%d", time.Now().UnixNano())
	srcCfg, dstCfg = srcAdmin, dstAdmin
	srcCfg.Database, dstCfg.Database = db, db
	for _, admin := range []connector.Config{srcAdmin, dstAdmin} {
		c, err := postgres.Connect(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
		t.Cleanup(func() {
			bg := context.Background()
			if c, err := postgres.Connect(bg, admin); err == nil {
				c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
				c.Close(bg)
			}
		})
	}
	var err error
	if src, err = postgres.Connect(ctx, srcCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close(context.Background()) })
	if _, err := src.PgConn().Exec(ctx, fixture+`CREATE MATERIALIZED VIEW customer_names AS SELECT name FROM customers;`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })

	// A target that holds none of the names passes, and lists nothing.
	report, err := Preflight(ctx, srcCfg, dstCfg, existingConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Existing objects"); c.Status != "ok" || !report.OK || len(report.Existing) != 0 {
		t.Fatalf("clean target: ok=%v, existing objects %s: %s, listed %v", report.OK, c.Status, c.Detail, report.Existing)
	}

	if _, err := dst.PgConn().Exec(ctx, `
		CREATE TYPE mood AS ENUM ('other');
		CREATE DOMAIN email AS integer;
		CREATE SEQUENCE change_log_id_seq;
		SELECT setval('change_log_id_seq', 777);
		CREATE FUNCTION log_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
		CREATE VIEW customer_totals AS SELECT 1 AS one;
		CREATE MATERIALIZED VIEW customer_names AS SELECT 2 AS two;
		CREATE SEQUENCE customers_name_idx;
		CREATE VIEW change_log_pkey AS SELECT 3 AS three;
		CREATE TABLE customers (id int);
		INSERT INTO customers VALUES (42);
		CREATE TRIGGER customers_log BEFORE DELETE ON customers FOR EACH ROW EXECUTE FUNCTION log_change();
	`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	return srcCfg, dstCfg, src, dst
}

// existingConfig is an offline job with the given policy for what the
// target already has.
func existingConfig(policy string) Config {
	return Config{Mode: migrate.ModeOffline, ExcludeTables: []string{"public.scratch"}, OnExisting: policy}
}

func findCheck(t *testing.T, r *Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("preflight has no %q check", name)
	return Check{}
}

// existingList is the preflight's list of what the target already has.
func existingList(r *Report) string {
	var out []string
	for _, e := range r.Existing {
		out = append(out, e.Kind+" "+e.Name)
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

// wantExisting is what the preflight must list on the target existingObjects
// prepares, each object under the kind and name the target has it by.
const wantExisting = "domain public.email; function public.log_change(); materialized view public.customer_names; " +
	"sequence public.change_log_id_seq; sequence public.customers_name_idx; table public.customers; " +
	"trigger customers_log on public.customers; type public.mood; view public.change_log_pkey; view public.customer_totals"

// preexisting describes the objects existingObjects put on the target, with
// what tells one definition of each from another.
func preexisting(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var s string
	if err := dst.QueryRow(ctx, `SELECT concat_ws(' | ',
		(SELECT string_agg(enumlabel, ',') FROM pg_enum WHERE enumtypid = 'mood'::regtype),
		(SELECT typbasetype::regtype::text FROM pg_type WHERE typname = 'email'),
		(SELECT last_value::text || is_called::text FROM change_log_id_seq),
		(SELECT string_agg(proname || ':' || md5(prosrc), ',' ORDER BY 1) FROM pg_proc WHERE pronamespace = 'public'::regnamespace),
		(SELECT string_agg(viewname || ':' || md5(definition), ',' ORDER BY 1) FROM pg_views WHERE schemaname = 'public'),
		(SELECT string_agg(matviewname || ':' || md5(definition), ',' ORDER BY 1) FROM pg_matviews),
		(SELECT string_agg(relname || ':' || relkind::text, ',' ORDER BY 1) FROM pg_class WHERE relnamespace = 'public'::regnamespace
			AND relname IN ('change_log_id_seq', 'customer_totals', 'customer_names', 'customers_name_idx', 'change_log_pkey', 'customers')),
		(SELECT string_agg(tgname || ':' || tgtype::text || ':' || tgrelid::regclass::text, ',' ORDER BY 1) FROM pg_trigger WHERE NOT tgisinternal),
		(SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'customers'::regclass AND attnum > 0),
		(SELECT string_agg(indexname, ',' ORDER BY 1) FROM pg_indexes WHERE tablename = 'customers'),
		(SELECT string_agg(conname, ',' ORDER BY 1) FROM pg_constraint WHERE conrelid = 'customers'::regclass OR confrelid = 'customers'::regclass),
		(SELECT string_agg(id::text, ',') FROM customers))`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMigrationExistingObjects is the default policy: preflight names every
// object that is in the way, and the job refuses to start and leaves them
// as they were.
func TestMigrationExistingObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := existingObjects(t, ctx)
	cfg := existingConfig("")
	before := preexisting(t, ctx, dst)

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		t.Logf("preflight %-28s %-4s %s", c.Name, c.Status, c.Detail)
	}
	if report.OK {
		t.Error("preflight passed over a target that has the migration's names taken")
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	if c := findCheck(t, report, "Target is empty"); c.Status != "fail" || !strings.Contains(c.Detail, "public.customers") {
		t.Errorf("target is empty: %s: %s", c.Status, c.Detail)
	}
	existing := findCheck(t, report, "Existing objects")
	if existing.Status != "fail" {
		t.Errorf("existing objects: %s", existing.Status)
	}
	for _, want := range []string{
		"types: public.mood", "domains: public.email", "sequences: public.change_log_id_seq",
		"functions: public.log_change()", "views: public.customer_totals", "materialized views: public.customer_names",
		"public.customers_name_idx (the name of a sequence)", "public.change_log_pkey (the name of a view)",
		"triggers: customers_log on public.customers", "never replaces or merges",
	} {
		if !strings.Contains(existing.Detail, want) {
			t.Errorf("existing objects does not mention %q", want)
		}
	}
	// What the source has and the target does not is not reported.
	for _, not := range []string{"billing", "customers_id_seq", "tables:"} {
		if strings.Contains(existing.Detail, not) {
			t.Errorf("existing objects mentions %q", not)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_exist1", cfg, State{}, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") ||
		!strings.Contains(err.Error(), "Existing objects") || !strings.Contains(err.Error(), "Target is empty") {
		t.Fatalf("run over existing objects: %v %v", out, err)
	}
	if after := preexisting(t, ctx, dst); after != before {
		t.Errorf("the refused job changed the target:\nbefore %s\nafter  %s", before, after)
	}
	var schemas string
	dst.QueryRow(ctx, `SELECT coalesce(string_agg(nspname, ','), '') FROM pg_namespace WHERE nspname IN ('billing', 'dedie')`).Scan(&schemas)
	if schemas != "" {
		t.Errorf("the refused job created %s on the target", schemas)
	}
}

// TestMigrationExistingSkip keeps what the target has. The table that was
// there is left out of the migration with everything that hangs on it, the
// other objects are neither created nor altered, and the rest arrives.
func TestMigrationExistingSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := existingObjects(t, ctx)
	cfg := existingConfig(migrate.ExistingSkip)
	before := preexisting(t, ctx, dst)

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Error("preflight failed although the job keeps what is there")
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	for _, name := range []string{"Target is empty", "Existing objects"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-28s %-4s %s", c.Name, c.Status, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "kept exactly as they are") || !strings.Contains(c.Detail, "left out of the migration") {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_skip1", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	if after := preexisting(t, ctx, dst); after != before {
		t.Errorf("the job changed what it was told to keep:\nbefore %s\nafter  %s", before, after)
	}
	_, st := rep.snapshot()
	if strings.Join(st.SkippedTables, ",") != "public.customers" || len(st.KeptObjects) != 9 || len(st.DropFirst) != 0 {
		t.Errorf("remembered: skipped %v, kept %v, drop %v", st.SkippedTables, st.KeptObjects, st.DropFirst)
	}
	if _, touched := rep.tables["public.customers"]; touched {
		t.Error("the kept table was copied or validated")
	}
	// Everything else arrived.
	for _, sql := range []string{
		`SELECT md5(string_agg(i::text, '' ORDER BY id)) FROM billing.invoices i`,
		`SELECT md5(string_agg(e::text, '' ORDER BY kind, payload)) FROM events e`,
		`SELECT count(*)::text FROM change_log`,
		`SELECT last_value::text FROM billing.invoices_id_seq`,
	} {
		var a, b string
		if err := src.QueryRow(ctx, sql).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", sql, err)
		}
		if err := dst.QueryRow(ctx, sql).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", sql, err)
		}
		if a != b {
			t.Errorf("%s: source %s, target %s", sql, a, b)
		}
	}
	for name, tbl := range rep.tables {
		if tbl["validation"] != "match" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}

	// A resumed job still knows what it left out, now that its own tables
	// exist as well: it validates the same tables and touches nothing else.
	out, err = Run(ctx, "mig_skip1", cfg, st, srcCfg, dstCfg, rep)
	if err != nil || out != Completed {
		t.Fatalf("resumed run: %v %v", out, err)
	}
	if after := preexisting(t, ctx, dst); after != before {
		t.Errorf("the resumed job changed what it was told to keep:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMigrationExistingReplace drops what the target has under the
// migration's names and puts the source's objects in their place.
func TestMigrationExistingReplace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := existingObjects(t, ctx)
	cfg := existingConfig(migrate.ExistingReplace)

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
	for name, object := range map[string]string{"Target is empty": "public.customers", "Existing objects": "public.log_change()"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-28s %-4s %s", c.Name, c.Status, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "DROPPED") || !strings.Contains(c.Detail, object) {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	var dropped bool
	rep := &eventReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(msg string) {
		dropped = dropped || strings.Contains(msg, "dropped 1 existing table(s) and 9 other object(s) on the target, as chosen")
	}}
	out, err := Run(ctx, "mig_replace1", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	if !dropped {
		t.Error("the job did not report what it dropped")
	}
	// The target now has what the source has under every one of the names.
	for _, sql := range []string{
		`SELECT string_agg(enumlabel, ',' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = 'mood'::regtype`,
		`SELECT typbasetype::regtype::text FROM pg_type WHERE typname = 'email'`,
		`SELECT last_value::text FROM change_log_id_seq`,
		`SELECT md5(prosrc) FROM pg_proc WHERE proname = 'log_change'`,
		`SELECT sum(total)::text FROM customer_totals`,
		`SELECT count(*)::text FROM customer_names`,
		`SELECT string_agg(relname || ':' || relkind::text, ',' ORDER BY relname COLLATE "C") FROM pg_class WHERE relname IN ('customers_name_idx', 'change_log_pkey')`,
		`SELECT string_agg(tgname || ':' || tgtype::text || ':' || tgrelid::regclass::text, ',') FROM pg_trigger WHERE NOT tgisinternal`,
		`SELECT md5(string_agg(c::text, '' ORDER BY id)) FROM customers c`,
		`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f'`,
	} {
		var a, b string
		if err := src.QueryRow(ctx, sql).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", sql, err)
		}
		if err := dst.QueryRow(ctx, sql).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", sql, err)
		}
		if a != b {
			t.Errorf("%s: source %s, target %s", sql, a, b)
		}
	}
	for name, tbl := range rep.tables {
		if tbl["validation"] != "match" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
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
	order := map[string]int{PhaseSchema: 1, PhaseIndexing: 2, PhaseCutover: 3}
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
	srcAdmin, dstAdmin := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_test_%d", time.Now().UnixNano())
	srcCfg, dstCfg := srcAdmin, dstAdmin
	srcCfg.Database, dstCfg.Database = db, db
	for _, admin := range []connector.Config{srcAdmin, dstAdmin} {
		c, err := postgres.Connect(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
		t.Cleanup(func() {
			bg := context.Background()
			if c, err := postgres.Connect(bg, admin); err == nil {
				c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
				c.Close(bg)
			}
		})
	}
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	if _, err := src.PgConn().Exec(ctx, fixture+`CREATE MATERIALIZED VIEW customer_names AS SELECT name FROM customers;`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)

	// Everything a job leaves behind on either side: the objects of the
	// user schemas and the checkpoint schema on the target, the slot and
	// the publication on the source.
	const stateSQL = `SELECT concat_ws(' | ',
		(SELECT string_agg(nspname, ',' ORDER BY 1) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
		(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
		(SELECT count(*) FROM pg_type y JOIN pg_namespace n ON n.oid = y.typnamespace WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
		(SELECT count(*) FROM pg_trigger), (SELECT count(*) FROM pg_constraint),
		(SELECT count(*) FROM pg_publication),
		(SELECT count(*) FROM pg_replication_slots WHERE database = current_database()))`
	state := func(conn *pgx.Conn) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, stateSQL).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	srcBefore, dstBefore := state(src), state(dst)
	if !strings.HasPrefix(dstBefore, "public | 0 | 0 | 0 | ") {
		t.Fatalf("the target is not empty: %s", dstBefore)
	}
	unchanged := func(when string) {
		t.Helper()
		if got := state(src); got != srcBefore {
			t.Errorf("%s changed the source:\nbefore %s\nafter  %s", when, srcBefore, got)
		}
		if got := state(dst); got != dstBefore {
			t.Errorf("%s changed the target:\nbefore %s\nafter  %s", when, dstBefore, got)
		}
	}

	cfg := Config{ExcludeTables: []string{"public.scratch"}}
	list, err := pipeline{}.Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	unchanged("the preview")
	var lines []string
	for _, s := range list {
		lines = append(lines, previewLine(s))
	}
	// The test account is a superuser and may apply changes in the replica
	// role, so the foreign key is built with the indexes; the trigger, the
	// identity column's return to ALWAYS and the refresh of the
	// materialized view wait for cutover.
	want := []string{
		"schema | schema | billing", "schema | schema | public",
		"schema | type | public.mood", "schema | domain | public.email",
		"schema | sequence | public.change_log_id_seq", "schema | sequence | public.customers_id_seq",
		"schema | function | public.log_change()",
		"schema | table | billing.invoices", "schema | table | public.change_log", "schema | table | public.customers", "schema | table | public.events",
		"schema | primary key | billing.invoices invoices_pkey", "schema | primary key | public.change_log change_log_pkey", "schema | primary key | public.customers customers_pkey",
		"schema | check constraint | billing.invoices invoices_amount_check",
		"schema | sequence owner | public.change_log_id_seq", "schema | sequence owner | public.customers_id_seq",
		"indexing | foreign key | billing.invoices invoices_customer_id_fkey",
		"indexing | index | public.customers_name_idx",
		"indexing | materialized view | public.customer_names", "indexing | view | public.customer_totals",
		"cutover | identity | billing.invoices.id", "cutover | refresh | public.customer_names",
		"cutover | trigger | customers_log on public.customers",
	}
	if got := strings.Join(lines, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("preview:\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	checkPhaseOrder(t, list)
	for _, exact := range []struct{ kind, name, sql string }{
		{"schema", "billing", `CREATE SCHEMA IF NOT EXISTS "billing"`},
		{"type", "public.mood", `CREATE TYPE public.mood AS ENUM ('sad', 'ok', 'happy')`},
		{"index", "public.customers_name_idx", `CREATE INDEX customers_name_idx ON public.customers USING btree (lower(name))`},
		{"foreign key", "billing.invoices invoices_customer_id_fkey",
			`ALTER TABLE ONLY billing.invoices ADD CONSTRAINT "invoices_customer_id_fkey" FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON UPDATE CASCADE ON DELETE CASCADE`},
		{"identity", "billing.invoices.id", `ALTER TABLE "billing"."invoices" ALTER COLUMN "id" SET GENERATED ALWAYS`},
	} {
		if s := findStatement(t, list, exact.kind, exact.name); s.SQL != exact.sql {
			t.Errorf("%s %s:\n got %s\nwant %s", exact.kind, exact.name, s.SQL, exact.sql)
		}
	}
	customers := findStatement(t, list, "table", "public.customers").SQL
	for _, part := range []string{`CREATE TABLE "public"."customers" (`, `"contact" public.email`, `"feeling" public.mood DEFAULT 'ok'::public.mood`,
		`"balance" numeric(12,2) DEFAULT 0`, `"name_upper" text GENERATED ALWAYS AS (upper(name)) STORED`} {
		if !strings.Contains(customers, part) {
			t.Errorf("CREATE TABLE of customers lacks %q:\n%s", part, customers)
		}
	}
	// A column that is GENERATED ALWAYS on the source is created BY DEFAULT
	// until cutover, and the preview shows it as it is sent.
	if s := findStatement(t, list, "table", "billing.invoices").SQL; !strings.Contains(s, `"id" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL`) {
		t.Errorf("CREATE TABLE of invoices:\n%s", s)
	}

	// An offline job builds its foreign keys with the indexes too, and a
	// table-level one leaves the views out.
	scoped, err := Preview(ctx, srcCfg, dstCfg, Config{Mode: migrate.ModeOffline, Tables: []string{"billing.invoices", "public.customers"}})
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, scoped)
	if s := findStatement(t, scoped, "foreign key", "billing.invoices invoices_customer_id_fkey"); s.Phase != PhaseIndexing {
		t.Errorf("offline foreign key in phase %s", s.Phase)
	}
	for _, s := range scoped {
		if s.Kind == "view" || s.Kind == "materialized view" || strings.Contains(s.Name, "change_log") && s.Kind == "table" {
			t.Errorf("table scope previews %s", previewLine(s))
		}
	}

	// What the target already has. From here on the previews depend on it.
	if _, err := dst.PgConn().Exec(ctx, `CREATE TABLE customers (id int); INSERT INTO customers VALUES (42);
		CREATE VIEW customer_totals AS SELECT 1 AS one;`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	dstBefore = state(dst)

	// Under the default policy the job would refuse to start; its preview
	// still shows every object as it would be created.
	again, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(list) {
		t.Error("the default policy previews something else once the target holds some of the names")
	}

	// Kept: the table, with all that belongs to it, and the view are left
	// out, and nothing is dropped.
	cfg.OnExisting = migrate.ExistingSkip
	skip, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, skip)
	for _, s := range skip {
		// The table's serial sequence is an object of the schema in its
		// own right and is still created.
		if s.Kind == "drop" || strings.Contains(s.Name, "customers") && s.Kind != "sequence" || s.Name == "public.customer_totals" {
			t.Errorf("skip previews %s: %s", previewLine(s), s.SQL)
		}
	}
	findStatement(t, skip, "table", "billing.invoices")
	findStatement(t, skip, "materialized view", "public.customer_names")
	if len(skip) >= len(list) {
		t.Errorf("skip previews %d statements, the whole migration %d", len(skip), len(list))
	}

	// Replaced: the drops come first, then the whole migration.
	cfg.OnExisting = migrate.ExistingReplace
	replace, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, replace)
	drops := []migrate.Statement{
		{Phase: PhaseSchema, Kind: "drop", Name: "public.customers", SQL: `DROP TABLE IF EXISTS public.customers CASCADE`},
		{Phase: PhaseSchema, Kind: "drop", Name: "public.customer_totals", SQL: `DROP VIEW IF EXISTS public.customer_totals CASCADE`},
	}
	if len(replace) != len(drops)+len(list) || fmt.Sprint(replace[:len(drops)]) != fmt.Sprint(drops) || fmt.Sprint(replace[len(drops):]) != fmt.Sprint(list) {
		t.Errorf("replace previews:\n%v", replace)
	}
	unchanged("the previews")

	// The preview is the script: every statement runs, in order, as it is.
	// Nothing in it depends on the rows the job would copy in between.
	for _, s := range replace {
		if _, err := dst.PgConn().Exec(ctx, s.SQL).ReadAll(); err != nil {
			t.Fatalf("%s: %v\n%s", previewLine(s), err, s.SQL)
		}
	}
	const shapeSQL = `SELECT concat_ws(' | ',
		(SELECT string_agg(n.nspname || '.' || c.relname || ':' || c.relkind::text, ',' ORDER BY n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname IN ('public', 'billing') AND c.relname <> 'scratch'),
		(SELECT string_agg(conname || ':' || contype::text, ',' ORDER BY conname) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
			WHERE n.nspname IN ('public', 'billing') AND c.contype IN ('p', 'u', 'c', 'f', 'x')),
		(SELECT string_agg(tgname, ',') FROM pg_trigger WHERE NOT tgisinternal),
		(SELECT string_agg(attrelid::regclass::text || '.' || attname || ':' || attidentity::text || attgenerated::text, ',' ORDER BY attrelid::regclass::text, attname) FROM pg_attribute
			WHERE attrelid IN ('public.customers'::regclass, 'billing.invoices'::regclass) AND attnum > 0))`
	var a, b string
	if err := src.QueryRow(ctx, shapeSQL).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRow(ctx, shapeSQL).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the script did not build the source's schema:\nsource %s\ntarget %s", a, b)
	}
}
