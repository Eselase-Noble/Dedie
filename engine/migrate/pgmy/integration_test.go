package pgmy

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// The integration test needs a PostgreSQL server with wal_level = logical
// and a MySQL 8 server:
//
//	DEDIE_TEST_SOURCE=postgres://postgres@127.0.0.1:54316/postgres
//	DEDIE_TEST_MYSQL_TARGET=mysql://root@127.0.0.1:33162
//
// It creates and drops its own database on both.

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
	state   pgpg.State
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
	m.state = s.(pgpg.State)
}
func (m *memReporter) ResetTables([]migrate.TableProgress) {}
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
func (m *memReporter) snapshot() (string, pgpg.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase, m.state
}

// The fixture covers the types that need conversion on the way: booleans,
// bytea, time zones, arrays, enums, UUIDs, unbounded numerics and text used
// in a key, plus a second schema, an identity column and a generated one.
const fixture = `
CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');
CREATE SCHEMA billing;
CREATE TABLE customers (
	id serial PRIMARY KEY, code text NOT NULL UNIQUE, name text NOT NULL, active boolean NOT NULL DEFAULT true,
	feeling mood DEFAULT 'ok', token uuid, profile jsonb, avatar bytea, tags text[], scores int[],
	balance numeric(12,2) DEFAULT 0, big numeric, ratio double precision, born date, wakes time,
	created timestamp DEFAULT now(), seen timestamptz, bio text,
	name_upper text GENERATED ALWAYS AS (upper(name)) STORED);
CREATE INDEX customers_name_idx ON customers (name);
CREATE INDEX customers_lower_idx ON customers (lower(name));
CREATE TABLE billing.invoices (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	customer_id int NOT NULL REFERENCES public.customers(id) ON UPDATE CASCADE ON DELETE CASCADE,
	amount numeric(12,2) NOT NULL, issued date DEFAULT current_date);
CREATE TABLE events (kind varchar(20), payload text);
ALTER TABLE events REPLICA IDENTITY FULL;
INSERT INTO customers (code, name, active, feeling, token, profile, avatar, tags, scores, balance, big, ratio, born, wakes, created, seen, bio)
	SELECT 'C' || g, 'user ' || g || ' ☃', g % 2 = 0, (ARRAY['sad','ok','happy'])[1 + g % 3]::mood, md5(g::text)::uuid,
		jsonb_build_object('n', g, 'tags', jsonb_build_array('x', 'y')), decode(md5(g::text), 'hex'),
		ARRAY['a', 'b c', 'quote"d', NULL], ARRAY[g, g * 2], g * 1.25, 12345678901234567890.5 + g, g / 7.0,
		'1990-05-17', '06:30:00', '2024-02-29 13:14:15.123456', '2024-02-29 13:14:15.5+02',
		E'tab\there, backslash \\ and\nnewline ' || repeat(md5(g::text), 50)
	FROM generate_series(1, 5000) g;
INSERT INTO billing.invoices (customer_id, amount) SELECT 1 + (g * 7) % 5000, g * 1.5 FROM generate_series(1, 5000) g;
INSERT INTO events SELECT 'k' || (g % 10), 'payload ' || g FROM generate_series(1, 1000) g;
`

func TestMigration(t *testing.T) {
	srcAdmin, dstCfg := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_y_%d", time.Now().UnixNano())
	pgAdmin, err := postgres.Connect(ctx, srcAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	myAdmin, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := myAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		myAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		myAdmin.Close()
		if c, err := postgres.Connect(bg, srcAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	srcCfg := srcAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	if _, err := src.PgConn().Exec(ctx, fixture).ReadAll(); err != nil {
		t.Fatal(err)
	}

	cfg := migrate.Config{FenceSource: true, CutoverTimeoutSeconds: 60}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Status != "ok" {
			t.Logf("preflight %-26s %-4s %s", c.Name, c.Status, strings.ReplaceAll(c.Detail, "\n", "\n    "))
		}
	}
	if !report.OK {
		t.Fatal("preflight failed")
	}

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
				fmt.Sprintf(`INSERT INTO customers (code, name, active, tags, scores, seen, bio) VALUES ('L%d', 'live %d', false, '{x,"y z"}', '{1,2,3}', now(), repeat('x', 5000))`, n, n),
				fmt.Sprintf(`UPDATE customers SET balance = balance + 1, feeling = 'happy', active = NOT active, avatar = '\x00ff10', tags = NULL WHERE id = %d`, i+1),
				fmt.Sprintf(`INSERT INTO billing.invoices (customer_id, amount) VALUES (%d, 9.99)`, i+1),
				fmt.Sprintf(`DELETE FROM billing.invoices WHERE id = %d`, n%5000+1),
				fmt.Sprintf(`UPDATE events SET payload = 'changed %d' WHERE payload = 'payload %d'`, n, i+1+round*200),
				// Cascades are replicated by PostgreSQL as ordinary changes.
				fmt.Sprintf(`UPDATE customers SET id = %d WHERE id = %d`, 900000-(i+round*200), 4000+i+round*200),
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
	run := func(st pgpg.State) (migrate.Outcome, error) { return Run(ctx, "mig_y1", cfg, st, srcCfg, dstCfg, rep) }
	type result struct {
		out migrate.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() { o, e := run(pgpg.State{}); done <- result{o, e} }()
	go workload(0)
	deadline := time.Now().Add(90 * time.Second)
	for {
		if p, _ := rep.snapshot(); p == migrate.PhaseStreaming {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for streaming")
		}
		select {
		case r := <-done:
			t.Fatalf("stopped early: %v %v", r.out, r.err)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Pause and resume: the stream must pick up exactly where it stopped.
	// The stream is only waited for by phase: on a shared test server the
	// first workload can finish before streaming begins.
	rep.SetDesired("pause")
	if r := <-done; r.err != nil || r.out != migrate.Paused {
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
		if r.err != nil || r.out != migrate.Completed {
			t.Fatalf("cutover: %v %v", r.out, r.err)
		}
	case <-ctx.Done():
		t.Fatal("cutover did not finish")
	}

	dst, err := mysql.Open(ctx, dstCfg, map[string]string{"group_concat_max_len": "1000000000", "information_schema_stats_expiry": "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := src.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		t.Fatal(err)
	}
	// Each pair computes the same figure in both dialects.
	check := func(what, pg, my string) {
		t.Helper()
		var a *string
		var b sql.NullString
		if err := src.QueryRow(ctx, pg).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", what, err)
		}
		if err := dst.QueryRowContext(ctx, my).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", what, err)
		}
		want, got := "NULL", "NULL"
		if a != nil {
			want = *a
		}
		if b.Valid {
			got = b.String
		}
		if want != got {
			t.Errorf("%s: source %.80s, target %.80s", what, want, got)
		} else {
			t.Logf("ok  %-30s %.60s", what, want)
		}
	}
	check("customers", `SELECT count(*)::text FROM customers`, `SELECT COUNT(*) FROM customers`)
	check("invoices (second schema)", `SELECT count(*)::text FROM billing.invoices`, `SELECT COUNT(*) FROM billing_invoices`)
	check("events", `SELECT count(*)::text FROM events`, `SELECT COUNT(*) FROM events`)
	check("text", `SELECT md5(string_agg(code || name, '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(CONCAT(code, name) ORDER BY id SEPARATOR '')) FROM customers`)
	check("long text with escapes", `SELECT md5(string_agg(md5(bio), '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(MD5(bio) ORDER BY id SEPARATOR '')) FROM customers`)
	check("boolean", `SELECT count(*)::text FROM customers WHERE active`, `SELECT COUNT(*) FROM customers WHERE active = 1`)
	check("enum", `SELECT md5(string_agg(feeling::text, '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(feeling ORDER BY id SEPARATOR '')) FROM customers`)
	check("uuid", `SELECT md5(string_agg(coalesce(token::text, '~'), '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(COALESCE(token, '~') ORDER BY id SEPARATOR '')) FROM customers`)
	check("bytea", `SELECT md5(string_agg(coalesce(encode(avatar, 'hex'), '~'), '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(COALESCE(LOWER(HEX(avatar)), '~') ORDER BY id SEPARATOR '')) FROM customers`)
	check("json", `SELECT sum((profile->>'n')::bigint)::text FROM customers`, `SELECT CAST(SUM(JSON_EXTRACT(profile, '$.n')) AS UNSIGNED) FROM customers`)
	check("text array", `SELECT count(*)::text FROM customers WHERE tags[2] = 'b c' AND tags[3] = 'quote"d' AND tags[4] IS NULL`,
		`SELECT COUNT(*) FROM customers WHERE JSON_UNQUOTE(JSON_EXTRACT(tags, '$[1]')) = 'b c' AND JSON_UNQUOTE(JSON_EXTRACT(tags, '$[2]')) = 'quote"d' AND JSON_TYPE(JSON_EXTRACT(tags, '$[3]')) = 'NULL'`)
	check("integer array", `SELECT sum(scores[1] + scores[2])::text FROM customers`, `SELECT CAST(SUM(JSON_EXTRACT(scores, '$[0]') + JSON_EXTRACT(scores, '$[1]')) AS UNSIGNED) FROM customers`)
	check("numeric", `SELECT sum(balance)::text FROM customers`, `SELECT CAST(SUM(balance) AS CHAR) FROM customers`)
	check("unbounded numeric", `SELECT trim(trailing '0' from sum(big)::text) FROM customers`, `SELECT TRIM(TRAILING '0' FROM CAST(SUM(big) AS CHAR)) FROM customers`)
	check("date and time", `SELECT count(*)::text FROM customers WHERE born = '1990-05-17' AND wakes = '06:30:00'`, `SELECT COUNT(*) FROM customers WHERE born = '1990-05-17' AND wakes = '06:30:00'`)
	check("timestamp", `SELECT count(*)::text FROM customers WHERE created = '2024-02-29 13:14:15.123456'`, `SELECT COUNT(*) FROM customers WHERE created = '2024-02-29 13:14:15.123456'`)
	check("timestamptz (UTC)", `SELECT count(*)::text FROM customers WHERE seen = '2024-02-29 11:14:15.5+00'`, `SELECT COUNT(*) FROM customers WHERE seen = '2024-02-29 11:14:15.5'`)
	check("cascaded invoices", `SELECT sum(customer_id)::text || '/' || sum(amount)::text FROM billing.invoices`, `SELECT CONCAT(SUM(customer_id), '/', SUM(amount)) FROM billing_invoices`)
	check("events after truncate", `SELECT string_agg(kind || payload, ',') FROM events`, `SELECT GROUP_CONCAT(CONCAT(kind, payload)) FROM events`)

	var indexes, fks, generated int
	dst.QueryRowContext(ctx, `SELECT COUNT(DISTINCT table_name, index_name) FROM information_schema.statistics WHERE table_schema = DATABASE()`).Scan(&indexes)
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema = DATABASE() AND constraint_type = 'FOREIGN KEY'`).Scan(&fks)
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND column_name = 'name_upper'`).Scan(&generated)
	if indexes < 4 || fks != 1 || generated != 0 {
		t.Errorf("target has %d indexes, %d foreign keys, %d generated columns", indexes, fks, generated)
	}
	// New rows on the target must get ids beyond every id the source used.
	res, err := dst.ExecContext(ctx, `INSERT INTO customers (code, name, active) VALUES ('post', 'post cutover', 1)`)
	if err != nil {
		t.Fatalf("insert on target after cutover: %v", err)
	}
	newID, _ := res.LastInsertId()
	var lastSeq int64
	src.QueryRow(ctx, `SELECT last_value FROM customers_id_seq`).Scan(&lastSeq)
	if newID <= lastSeq {
		t.Errorf("AUTO_INCREMENT continued at %d, at or below the source sequence's %d", newID, lastSeq)
	}
	var leftover, slots int
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, checkpointTable).Scan(&leftover)
	src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'dedie_mig_y1'`).Scan(&slots)
	if leftover != 0 || slots != 0 {
		t.Errorf("left behind: %d checkpoint table(s) on the target, %d slot(s) on the source", leftover, slots)
	}
	for name, tbl := range rep.tables {
		if v, _ := tbl["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
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
	srcAdmin, dstCfg := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_y_%d", time.Now().UnixNano())
	pgAdmin, err := postgres.Connect(ctx, srcAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	myAdmin, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := myAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		myAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		myAdmin.Close()
		if c, err := postgres.Connect(bg, srcAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	srcCfg := srcAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	// notes has no key at all: a stream could not follow it, a copy can.
	if _, err := src.PgConn().Exec(ctx, fixture+`CREATE TABLE notes (body text); INSERT INTO notes SELECT 'note ' || g FROM generate_series(1, 100) g;`).ReadAll(); err != nil {
		t.Fatal(err)
	}

	// Fencing is asked for and must be ignored: an offline job has no cutover.
	cfg := migrate.Config{Mode: migrate.ModeOffline, FenceSource: true}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, c := range report.Checks {
		switch c.Name {
		case "Offline copy":
			noted = c.Status == "warn"
		case "Source wal_level", "Replication privilege", "Replication capacity", "Tables without a key":
			t.Errorf("offline preflight should not check %q", c.Name)
		}
		if c.Status == "fail" {
			t.Errorf("preflight %s: %s", c.Name, c.Detail)
		}
	}
	if !report.OK || !noted {
		t.Fatalf("offline preflight: ok=%v, offline note=%v", report.OK, noted)
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_offline1", cfg, pgpg.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("offline run: %v %v", out, err)
	}
	phase, st := rep.snapshot()
	if phase != migrate.PhaseDone || rep.Desired() != "run" {
		t.Errorf("finished in phase %q with the operator's instruction %q", phase, rep.Desired())
	}
	if st.Slot != "" || st.Fenced {
		t.Errorf("offline state names slot %q, fenced %v", st.Slot, st.Fenced)
	}

	dst, err := mysql.Open(ctx, dstCfg, map[string]string{"group_concat_max_len": "1000000000", "information_schema_stats_expiry": "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	check := func(what, pg, my string) {
		t.Helper()
		var a *string
		var b sql.NullString
		if err := src.QueryRow(ctx, pg).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", what, err)
		}
		if err := dst.QueryRowContext(ctx, my).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", what, err)
		}
		if a == nil || *a != b.String {
			t.Errorf("%s: source %.80v, target %.80s", what, a, b.String)
		}
	}
	check("customers", `SELECT count(*)::text FROM customers`, `SELECT COUNT(*) FROM customers`)
	check("text", `SELECT md5(string_agg(code || name, '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(CONCAT(code, name) ORDER BY id SEPARATOR '')) FROM customers`)
	check("long text", `SELECT md5(string_agg(md5(bio), '' ORDER BY id)) FROM customers`, `SELECT MD5(GROUP_CONCAT(MD5(bio) ORDER BY id SEPARATOR '')) FROM customers`)
	check("numeric", `SELECT sum(balance)::text FROM customers`, `SELECT CAST(SUM(balance) AS CHAR) FROM customers`)
	check("invoices", `SELECT sum(customer_id)::text || '/' || sum(amount)::text FROM billing.invoices`, `SELECT CONCAT(SUM(customer_id), '/', SUM(amount)) FROM billing_invoices`)
	check("events", `SELECT count(*)::text FROM events`, `SELECT COUNT(*) FROM events`)
	check("table without a key", `SELECT md5(string_agg(body, '' ORDER BY body)) FROM notes`, `SELECT MD5(GROUP_CONCAT(body ORDER BY body SEPARATOR '')) FROM notes`)

	var fks int
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema = DATABASE() AND constraint_type = 'FOREIGN KEY'`).Scan(&fks)
	if fks != 1 {
		t.Errorf("target has %d foreign keys", fks)
	}
	for name, tbl := range rep.tables {
		if v, _ := tbl["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}
	var leftover, slots, pubs int
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, checkpointTable).Scan(&leftover)
	src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE database = current_database()`).Scan(&slots)
	src.QueryRow(ctx, `SELECT count(*) FROM pg_publication`).Scan(&pubs)
	if leftover != 0 || slots != 0 || pubs != 0 {
		t.Errorf("left behind: %d checkpoint table(s) on the target, %d slot(s) and %d publication(s) on the source", leftover, slots, pubs)
	}

	// The source stays writable. A job resumed after its copy goes straight
	// to validation, which must now fail: nothing carries this row over.
	if _, err := src.Exec(ctx, `INSERT INTO notes VALUES ('late write')`); err != nil {
		t.Fatalf("source refused a write after an offline job: %v", err)
	}
	out, err = Run(ctx, "mig_offline1", cfg, st, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("resumed offline job over a changed source: %v %v", out, err)
	}
	if phase, _ := rep.snapshot(); phase == migrate.PhaseStreaming {
		t.Error("offline job went to streaming")
	}
}

// existingObjects prepares a source and a target on which the names the
// migration wants are taken. Besides tables those are the foreign keys:
// MySQL names them per database, so one on an unrelated table is in the
// way. A view holds one table's name.
func existingObjects(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, dst *sql.DB) {
	t.Helper()
	srcAdmin, dstCfg := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	db := fmt.Sprintf("dedie_y_%d", time.Now().UnixNano())
	pgAdmin, err := postgres.Connect(ctx, srcAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	myAdmin, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := myAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		myAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		myAdmin.Close()
		if c, err := postgres.Connect(bg, srcAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	srcCfg = srcAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.PgConn().Exec(ctx, `
		CREATE TABLE customers (id int PRIMARY KEY, name text NOT NULL);
		CREATE TABLE invoices (id int PRIMARY KEY, customer_id int NOT NULL REFERENCES customers (id));
		CREATE TABLE events (id int PRIMARY KEY, kind text);
		INSERT INTO customers VALUES (1, 'one');
		INSERT INTO invoices VALUES (1, 1);
		INSERT INTO events VALUES (7, 'seven');
	`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	src.Close(ctx)

	// A target that holds none of the names passes, and lists nothing.
	report, err := Preflight(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Existing objects"); c.Status != "ok" || !report.OK || len(report.Existing) != 0 {
		t.Fatalf("clean target: ok=%v, existing objects %s: %s, listed %v", report.OK, c.Status, c.Detail, report.Existing)
	}

	if dst, err = mysql.Open(ctx, dstCfg, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	for _, stmt := range []string{
		`CREATE TABLE unrelated_parent (id INT PRIMARY KEY) ENGINE=InnoDB`,
		`CREATE TABLE unrelated_child (parent_id INT,
			CONSTRAINT invoices_invoices_customer_id_fkey FOREIGN KEY (parent_id) REFERENCES unrelated_parent (id)) ENGINE=InnoDB`,
		`CREATE TABLE events (other INT) ENGINE=InnoDB`,
		`INSERT INTO events VALUES (42)`,
		`CREATE VIEW customers AS SELECT 1 AS one`,
	} {
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	return srcCfg, dstCfg, dst
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

// wantExisting is what the preflight must list on the target existingObjects
// prepares, each object under the kind and name the target has it by.
const wantExisting = "foreign key invoices_invoices_customer_id_fkey on unrelated_child; table events; view customers"

// definitions describes the tables, views and constraints of the target
// database, and the columns of the tables whose names were taken.
func definitions(t *testing.T, ctx context.Context, dst *sql.DB) string {
	t.Helper()
	rows, err := dst.QueryContext(ctx, `SELECT CONCAT('table ', table_name, ' ', table_type) FROM information_schema.tables WHERE table_schema = DATABASE()
		UNION ALL SELECT CONCAT('column ', table_name, '.', column_name) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name IN ('events', 'customers')
		UNION ALL SELECT CONCAT('view ', table_name, ' ', view_definition) FROM information_schema.views WHERE table_schema = DATABASE()
		UNION ALL SELECT CONCAT('constraint ', constraint_name, ' ', constraint_type, ' on ', table_name) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE()
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// TestMigrationExistingObjects is the default policy: preflight names every
// object that is in the way, and the job refuses to start and leaves them
// as they were.
func TestMigrationExistingObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srcCfg, dstCfg, dst := existingObjects(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline}
	before := definitions(t, ctx, dst)

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Status == "fail" {
			t.Logf("preflight %-26s %-4s %s", c.Name, c.Status, c.Detail)
		}
	}
	if report.OK {
		t.Error("preflight passed over a target that has the migration's names taken")
	}
	if got := existingList(report); got != wantExisting {
		t.Errorf("existing objects listed:\n got %s\nwant %s", got, wantExisting)
	}
	if c := findCheck(t, report, "Target is empty"); c.Status != "fail" || !strings.Contains(c.Detail, "events") {
		t.Errorf("target is empty: %s: %s", c.Status, c.Detail)
	}
	existing := findCheck(t, report, "Existing objects")
	if existing.Status != "fail" {
		t.Errorf("existing objects: %s", existing.Status)
	}
	// A view holds a table's name as firmly as a table does.
	for _, want := range []string{
		"tables: customers (the name of a view)", "foreign keys: invoices_invoices_customer_id_fkey on unrelated_child", "never replaces or merges",
	} {
		if !strings.Contains(existing.Detail, want) {
			t.Errorf("existing objects does not mention %q", want)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_exist1", cfg, pgpg.State{}, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") ||
		!strings.Contains(err.Error(), "Existing objects") || !strings.Contains(err.Error(), "Target is empty") {
		t.Fatalf("run over existing objects: %v %v", out, err)
	}
	if after := definitions(t, ctx, dst); after != before {
		t.Errorf("the refused job changed the target:\nbefore\n%s\nafter\n%s", before, after)
	}
	var held string
	if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(other) FROM events`).Scan(&held); err != nil || held != "42" {
		t.Errorf("the table that was there holds %q: %v", held, err)
	}
}

// TestMigrationExistingSkip keeps what the target has. The tables whose
// names are taken are left out of the migration, the foreign key that would
// take a name in use is not created, and the rest arrives.
func TestMigrationExistingSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, dst := existingObjects(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}
	before := definitions(t, ctx, dst)

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
		t.Logf("preflight %-26s %-4s %s", c.Name, c.Status, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "kept exactly as they are") || !strings.Contains(c.Detail, "left out of the migration") {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_skip1", cfg, pgpg.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	// Only the one table whose name was free is new.
	want := strings.Replace(before, "table events BASE TABLE\n", "table events BASE TABLE\ntable invoices BASE TABLE\n", 1)
	want = strings.Replace(want, "constraint PRIMARY PRIMARY KEY on unrelated_parent", "constraint PRIMARY PRIMARY KEY on invoices\nconstraint PRIMARY PRIMARY KEY on unrelated_parent", 1)
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after keeping what was there:\nwant\n%s\ngot\n%s", want, after)
	}
	var held string
	if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(other) FROM events`).Scan(&held); err != nil || held != "42" {
		t.Errorf("the table that was there holds %q: %v", held, err)
	}
	_, st := rep.snapshot()
	sort.Strings(st.SkippedTables)
	if strings.Join(st.SkippedTables, ",") != "public.customers,public.events" ||
		strings.Join(st.KeptObjects, ",") != "foreign key invoices_invoices_customer_id_fkey" || len(st.DropFirst) != 0 {
		t.Errorf("remembered: skipped %v, kept %v, drop %v", st.SkippedTables, st.KeptObjects, st.DropFirst)
	}
	if v, _ := rep.tables["public.invoices"]["validation"].(string); len(rep.tables) != 1 || !strings.HasSuffix(v, "match") {
		t.Errorf("tables copied and validated: %v", rep.tables)
	}

	// A resumed job still knows what it left out, now that its own table
	// exists as well.
	out, err = Run(ctx, "mig_skip1", cfg, st, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("resumed run: %v %v", out, err)
	}
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("the resumed job changed the target:\nwant\n%s\ngot\n%s", want, after)
	}
}

// TestMigrationExistingReplace drops what the target has under the
// migration's names and puts the source's tables in their place.
func TestMigrationExistingReplace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, dst := existingObjects(t, ctx)
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
	for name, object := range map[string]string{"Target is empty": "events", "Existing objects": "invoices_invoices_customer_id_fkey on unrelated_child"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-26s %-4s %s", c.Name, c.Status, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "DROPPED") || !strings.Contains(c.Detail, object) {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	var dropped bool
	rep := &eventReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(msg string) {
		dropped = dropped || strings.Contains(msg, "dropped 1 existing table(s) and 2 other object(s) on the target, as chosen")
	}}
	out, err := Run(ctx, "mig_replace1", cfg, pgpg.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	if !dropped {
		t.Error("the job did not report what it dropped")
	}
	want := strings.Join([]string{
		"column customers.id", "column customers.name", "column events.id", "column events.kind",
		"constraint invoices_invoices_customer_id_fkey FOREIGN KEY on invoices",
		"constraint PRIMARY PRIMARY KEY on customers", "constraint PRIMARY PRIMARY KEY on events",
		"constraint PRIMARY PRIMARY KEY on invoices", "constraint PRIMARY PRIMARY KEY on unrelated_parent",
		"table customers BASE TABLE", "table events BASE TABLE", "table invoices BASE TABLE",
		"table unrelated_child BASE TABLE", "table unrelated_parent BASE TABLE",
	}, "\n")
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after replacing:\nwant\n%s\ngot\n%s", want, after)
	}
	var held string
	if err := dst.QueryRowContext(ctx, `SELECT CONCAT((SELECT GROUP_CONCAT(CONCAT(id, kind)) FROM events), ' ', (SELECT GROUP_CONCAT(name) FROM customers))`).Scan(&held); err != nil || held != "7seven one" {
		t.Errorf("the replaced tables hold %q: %v", held, err)
	}
	for name, tbl := range rep.tables {
		if v, _ := tbl["validation"].(string); !strings.HasSuffix(v, "match") || v == "mismatch" {
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
