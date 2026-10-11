package pgpg

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

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
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
	waitFor("streaming", func(p string, st State) bool { return p == PhaseStreaming && st.TxApplied > 0 })

	// Pause and resume: the stream must pick up exactly where it stopped.
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
	src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'dedie_%'`).Scan(&slots)
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
