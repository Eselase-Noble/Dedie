package mypg

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

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

// The integration test needs a MySQL server with binary logging in ROW
// format and a PostgreSQL server:
//
//	DEDIE_TEST_MYSQL_SOURCE=mysql://root@127.0.0.1:33161
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
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
	state   mymy.State
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
	m.state = s.(mymy.State)
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
func (m *memReporter) snapshot() (string, mymy.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase, m.state
}

// The fixture covers the types that need conversion on the way: unsigned
// integers, ENUM and SET, JSON, BIT, binary data, text in latin1, the
// temporal types including a zero date, and a generated column.
var fixture = []string{
	`CREATE TABLE customers (
		id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL,
		legacy_name VARCHAR(100) CHARACTER SET latin1, feeling ENUM('sad','ok','happy') DEFAULT 'ok',
		tags SET('a','b','c'), profile JSON, avatar BLOB, flags BIT(8), small TINYINT UNSIGNED, active TINYINT(1) DEFAULT 1,
		medium MEDIUMINT UNSIGNED, big BIGINT UNSIGNED, balance DECIMAL(12,2) DEFAULT 0, ratio DOUBLE,
		born DATE, wakes TIME, yr YEAR, created DATETIME(6), seen TIMESTAMP NULL, bio LONGTEXT,
		updated TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		name_upper VARCHAR(100) GENERATED ALWAYS AS (UPPER(name)) STORED,
		KEY name_idx (name), UNIQUE KEY big_small (big, small, id)) ENGINE=InnoDB`,
	`CREATE TABLE invoices (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, customer_id INT UNSIGNED NOT NULL, amount DECIMAL(12,2) NOT NULL,
		CONSTRAINT invoices_customer FOREIGN KEY (customer_id) REFERENCES customers (id) ON UPDATE CASCADE ON DELETE CASCADE) ENGINE=InnoDB`,
	`CREATE TABLE events (kind VARCHAR(20), payload VARCHAR(100), at DATETIME) ENGINE=InnoDB`,
	`CREATE VIEW customer_totals AS SELECT c.id, COALESCE(SUM(i.amount), 0) AS total FROM customers c LEFT JOIN invoices i ON i.customer_id = c.id GROUP BY c.id`,
	`SET SESSION cte_max_recursion_depth = 100000`,
	`SET SESSION sql_mode = ''`,
	`INSERT INTO customers (name, legacy_name, feeling, tags, profile, avatar, flags, small, medium, big, balance, ratio, born, wakes, yr, created, seen, bio)
		WITH RECURSIVE seq (g) AS (SELECT 1 UNION ALL SELECT g + 1 FROM seq WHERE g < 5000)
		SELECT CONCAT('user ', g, ' ☃'), CONCAT('caf', CHAR(233 USING latin1), ' ', g), ELT(1 + g % 3, 'sad', 'ok', 'happy'), 'a,c',
			JSON_OBJECT('n', g, 'tags', JSON_ARRAY('x', 'y')), UNHEX(MD5(g)), b'10100101', 250, 16000000, 18446744073709551000 + g % 600,
			g * 1.25, g / 7, IF(g % 50 = 0, '0000-00-00', '1990-05-17'), '06:30:00', 2024, '2024-02-29 13:14:15.123456', '2024-02-29 13:14:15',
			CONCAT('tab\there, backslash \\ and\nnewline ', REPEAT(MD5(g), 50))
		FROM seq`,
	`INSERT INTO invoices (customer_id, amount) SELECT 1 + (id * 7) % 5000, id * 1.5 FROM customers`,
	`INSERT INTO events SELECT CONCAT('k', id % 10), CONCAT('payload ', id), '2024-01-01 00:00:00' FROM customers WHERE id <= 1000`,
}

func TestMigration(t *testing.T) {
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_x_%d", time.Now().UnixNano())
	srcAdmin, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srcAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	pgAdmin, err := postgres.Connect(ctx, dstAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db+" ENCODING 'UTF8' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		srcAdmin.Exec("ALTER DATABASE " + db + " READ ONLY = 0")
		srcAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		srcAdmin.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	dstCfg := dstAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := mysql.Open(ctx, srcCfg, map[string]string{"group_concat_max_len": "1000000000", "information_schema_stats_expiry": "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	setup, err := src.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range fixture {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	setup.Close()

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
		for i := 0; i < 200; i++ {
			n := round*1000 + i
			stmts := []string{
				fmt.Sprintf(`INSERT INTO customers (name, legacy_name, feeling, tags, profile, big, small, flags, bio) VALUES ('live %d', CONCAT('na', CHAR(239 USING latin1), 've'), 'happy', 'b', '{"live": true}', 18446744073709551615, 255, b'00000011', REPEAT('x', 5000))`, n),
				fmt.Sprintf(`UPDATE customers SET balance = balance + 1, feeling = 'sad', tags = 'a,b,c', legacy_name = NULL, avatar = UNHEX('00FF10') WHERE id = %d`, i+1),
				fmt.Sprintf(`INSERT INTO invoices (customer_id, amount) VALUES (%d, 9.99)`, i+1),
				fmt.Sprintf(`DELETE FROM invoices WHERE id = %d`, n%5000+1),
				fmt.Sprintf(`UPDATE events SET payload = 'changed %d' WHERE payload = 'payload %d'`, n, i+1+round*200),
				fmt.Sprintf(`UPDATE customers SET id = %d WHERE id = %d`, 900000-(i+round*200), 4000+i+round*200),
				fmt.Sprintf(`DELETE FROM customers WHERE id = %d`, 3000+i+round*200),
			}
			for _, s := range stmts {
				if _, err := src.ExecContext(ctx, s); err != nil {
					t.Errorf("workload: %v", err)
					return
				}
			}
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	run := func(st mymy.State) (migrate.Outcome, error) { return Run(ctx, "mig_x1", cfg, st, srcCfg, dstCfg, rep) }
	type result struct {
		out migrate.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() { o, e := run(mymy.State{}); done <- result{o, e} }()
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
	if _, err := src.ExecContext(ctx, `TRUNCATE TABLE events`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ExecContext(ctx, `INSERT INTO events VALUES ('after', 'truncate', NULL)`); err != nil {
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

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)
	if _, err := dst.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		t.Fatal(err)
	}
	// Each pair computes the same figure in both dialects.
	check := func(what, my, pg string) {
		t.Helper()
		var a sql.NullString
		var b *string
		if err := src.QueryRowContext(ctx, my).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", what, err)
		}
		if err := dst.QueryRow(ctx, pg).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", what, err)
		}
		got := "NULL"
		if b != nil {
			got = *b
		}
		want := "NULL"
		if a.Valid {
			want = a.String
		}
		if want != got {
			t.Errorf("%s: source %.80s, target %.80s", what, want, got)
		} else {
			t.Logf("ok  %-34s %.60s", what, want)
		}
	}
	check("customers", `SELECT COUNT(*) FROM customers`, `SELECT count(*)::text FROM customers`)
	check("invoices", `SELECT COUNT(*) FROM invoices`, `SELECT count(*)::text FROM invoices`)
	check("events", `SELECT COUNT(*) FROM events`, `SELECT count(*)::text FROM events`)
	check("names (utf8mb4)", `SELECT MD5(GROUP_CONCAT(name ORDER BY id SEPARATOR ''))  FROM customers`, `SELECT md5(string_agg(name, '' ORDER BY id)) FROM customers`)
	check("names (latin1 to utf8)", `SELECT MD5(GROUP_CONCAT(CONVERT(COALESCE(legacy_name, '~') USING utf8mb4) ORDER BY id SEPARATOR ''))  FROM customers`,
		`SELECT md5(string_agg(coalesce(legacy_name, '~'), '' ORDER BY id)) FROM customers`)
	check("enum and set", `SELECT MD5(GROUP_CONCAT(CONCAT(feeling, ':', COALESCE(tags, '~')) ORDER BY id SEPARATOR ';')) FROM customers`,
		`SELECT md5(string_agg(feeling || ':' || coalesce(tags, '~'), ';' ORDER BY id)) FROM customers`)
	check("binary", `SELECT MD5(GROUP_CONCAT(LOWER(HEX(avatar)) ORDER BY id SEPARATOR ''))  FROM customers WHERE avatar IS NOT NULL`,
		`SELECT md5(string_agg(encode(avatar, 'hex'), '' ORDER BY id)) FROM customers WHERE avatar IS NOT NULL`)
	check("long text with escapes", `SELECT MD5(GROUP_CONCAT(MD5(bio) ORDER BY id SEPARATOR ''))  FROM customers`, `SELECT md5(string_agg(md5(bio), '' ORDER BY id)) FROM customers`)
	check("json", `SELECT CAST(SUM(JSON_EXTRACT(profile, '$.n')) AS UNSIGNED) FROM customers`, `SELECT sum((profile->>'n')::bigint)::text FROM customers`)
	check("unsigned bigint", `SELECT CAST(SUM(big) AS CHAR) FROM customers`, `SELECT sum(big)::text FROM customers`)
	check("integers and bit", `SELECT CAST(SUM(small + medium + flags + active + yr) AS CHAR) FROM customers`, `SELECT sum(small + medium + flags + active + yr)::text FROM customers`)
	check("decimal", `SELECT CAST(SUM(balance) AS CHAR) FROM customers`, `SELECT sum(balance)::text FROM customers`)
	check("zero dates become NULL", `SELECT COUNT(*) FROM customers WHERE CAST(born AS CHAR) = '0000-00-00'`, `SELECT count(*)::text FROM customers WHERE born IS NULL AND name LIKE 'user %'`)
	check("dates", `SELECT COUNT(*) FROM customers WHERE born = '1990-05-17' AND wakes = '06:30:00'`, `SELECT count(*)::text FROM customers WHERE born = '1990-05-17' AND wakes = '06:30:00'`)
	check("datetime", `SELECT COUNT(*) FROM customers WHERE created = '2024-02-29 13:14:15.123456'`, `SELECT count(*)::text FROM customers WHERE created = '2024-02-29 13:14:15.123456'`)
	check("timestamp (UTC)", `SELECT COUNT(*) FROM customers WHERE seen = '2024-02-29 13:14:15'`, `SELECT count(*)::text FROM customers WHERE seen = '2024-02-29 13:14:15+00'`)
	check("timestamp default", `SELECT MD5(GROUP_CONCAT(DATE_FORMAT(updated, '%Y-%m-%d %H:%i:%s') ORDER BY id SEPARATOR ''))  FROM customers`,
		`SELECT md5(string_agg(to_char(updated, 'YYYY-MM-DD HH24:MI:SS'), '' ORDER BY id)) FROM customers`)
	check("generated column", `SELECT MD5(GROUP_CONCAT(name_upper ORDER BY id SEPARATOR ''))  FROM customers`, `SELECT md5(string_agg(name_upper, '' ORDER BY id)) FROM customers`)
	check("cascaded invoices", `SELECT CONCAT(SUM(customer_id), '/', SUM(amount)) FROM invoices`, `SELECT sum(customer_id)::text || '/' || sum(amount)::text FROM invoices`)
	check("events after truncate", `SELECT GROUP_CONCAT(CONCAT(kind, payload))  FROM events`, `SELECT string_agg(kind || payload, ',') FROM events`)

	var indexes, fks int
	dst.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'`).Scan(&indexes)
	dst.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`).Scan(&fks)
	if indexes < 5 || fks != 1 {
		t.Errorf("target has %d indexes and %d foreign keys", indexes, fks)
	}
	// New rows on the target must get ids beyond every id the source used.
	var newID, maxID int64
	if err := dst.QueryRow(ctx, `INSERT INTO customers (name) VALUES ('post cutover') RETURNING id`).Scan(&newID); err != nil {
		t.Errorf("insert on target after cutover: %v", err)
	}
	src.QueryRowContext(ctx, `SELECT MAX(id) FROM customers`).Scan(&maxID)
	if newID <= maxID {
		t.Errorf("identity restarted at %d, at or below the source's highest id %d", newID, maxID)
	}
	var leftover bool
	dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'dedie')`).Scan(&leftover)
	if leftover {
		t.Error("checkpoint schema left on the target")
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

// TestMigrationOffline runs a one-time copy with a source account that may
// only read: no replication privilege, and no RELOAD either, so the copy
// falls back to a single snapshot connection. The job must finish by itself,
// with no cutover requested.
func TestMigrationOffline(t *testing.T) {
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_x_%d", time.Now().UnixNano())
	reader := fmt.Sprintf("dedie_r%d", time.Now().UnixNano()%1e9)
	srcAdmin, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srcAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	pgAdmin, err := postgres.Connect(ctx, dstAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db+" ENCODING 'UTF8' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		srcAdmin.Exec("DROP USER IF EXISTS " + reader)
		srcAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		srcAdmin.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	dstCfg := dstAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := mysql.Open(ctx, srcCfg, map[string]string{"group_concat_max_len": "1000000000", "information_schema_stats_expiry": "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	setup, err := src.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range append(fixture[:len(fixture):len(fixture)],
		"CREATE USER "+reader+" IDENTIFIED BY 'offline-only'",
		"GRANT SELECT, SHOW VIEW ON *.* TO "+reader) {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	setup.Close()
	readCfg := srcCfg
	readCfg.Username, readCfg.Password = reader, "offline-only"

	cfg := migrate.Config{Mode: migrate.ModeOffline}
	report, err := Preflight(ctx, readCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, c := range report.Checks {
		switch c.Name {
		case "Offline copy":
			noted = c.Status == "warn"
		case "Binary logging", "Binary log format", "Row images", "Replication privileges", "Tables without a primary key":
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
	out, err := Run(ctx, "mig_offline1", cfg, mymy.State{}, readCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("offline run: %v %v", out, err)
	}
	phase, st := rep.snapshot()
	if phase != migrate.PhaseDone || rep.Desired() != "run" {
		t.Errorf("finished in phase %q with the operator's instruction %q", phase, rep.Desired())
	}
	if st.Start.File != "" || st.Fenced {
		t.Errorf("offline state holds binary log position %q, fenced %v", st.Start, st.Fenced)
	}

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)
	check := func(what, my, pg string) {
		t.Helper()
		var a sql.NullString
		var b *string
		if err := src.QueryRowContext(ctx, my).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", what, err)
		}
		if err := dst.QueryRow(ctx, pg).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", what, err)
		}
		if b == nil || a.String != *b {
			t.Errorf("%s: source %.80s, target %.80v", what, a.String, b)
		}
	}
	check("customers", `SELECT COUNT(*) FROM customers`, `SELECT count(*)::text FROM customers`)
	check("events", `SELECT COUNT(*) FROM events`, `SELECT count(*)::text FROM events`)
	check("names", `SELECT MD5(GROUP_CONCAT(name ORDER BY id SEPARATOR ''))  FROM customers`, `SELECT md5(string_agg(name, '' ORDER BY id)) FROM customers`)
	check("long text", `SELECT MD5(GROUP_CONCAT(MD5(bio) ORDER BY id SEPARATOR ''))  FROM customers`, `SELECT md5(string_agg(md5(bio), '' ORDER BY id)) FROM customers`)
	check("unsigned bigint", `SELECT CAST(SUM(big) AS CHAR) FROM customers`, `SELECT sum(big)::text FROM customers`)
	check("invoices", `SELECT CONCAT(SUM(customer_id), '/', SUM(amount)) FROM invoices`, `SELECT sum(customer_id)::text || '/' || sum(amount)::text FROM invoices`)

	var fks int
	dst.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`).Scan(&fks)
	if fks != 1 {
		t.Errorf("target has %d foreign keys", fks)
	}
	var newID, maxID int64
	if err := dst.QueryRow(ctx, `INSERT INTO customers (name) VALUES ('after the copy') RETURNING id`).Scan(&newID); err != nil {
		t.Errorf("insert on target after the copy: %v", err)
	}
	src.QueryRowContext(ctx, `SELECT MAX(id) FROM customers`).Scan(&maxID)
	if newID <= maxID {
		t.Errorf("identity restarted at %d, at or below the source's highest id %d", newID, maxID)
	}
	var leftover bool
	dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'dedie')`).Scan(&leftover)
	if leftover {
		t.Error("checkpoint schema left on the target")
	}
	for name, tbl := range rep.tables {
		if v, _ := tbl["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}

	// A job resumed after its copy goes straight to validation, which must
	// now fail: the insert above made the two sides differ, and no stream
	// reconciles them.
	out, err = Run(ctx, "mig_offline1", cfg, st, readCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("resumed offline job over changed tables: %v %v", out, err)
	}
	if phase, _ := rep.snapshot(); phase == migrate.PhaseStreaming {
		t.Error("offline job went to streaming")
	}
}

// existingObjects prepares a source and a target on which the names the
// migration wants for its tables and indexes are taken. An index is named
// within its schema in PostgreSQL, so one on an unrelated table is in the
// way, and so is any other relation of that name. A type holds a table's
// name without being a table.
func existingObjects(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, dst *pgx.Conn) {
	t.Helper()
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	db := fmt.Sprintf("dedie_x_%d", time.Now().UnixNano())
	srcAdmin, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srcAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	pgAdmin, err := postgres.Connect(ctx, dstAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db+" ENCODING 'UTF8' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		srcAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		srcAdmin.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	dstCfg = dstAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE customers (id INT NOT NULL PRIMARY KEY, name VARCHAR(100) NOT NULL, KEY name_idx (name)) ENGINE=InnoDB`,
		`CREATE TABLE invoices (id INT NOT NULL PRIMARY KEY, amount DECIMAL(12,2), KEY amount_idx (amount)) ENGINE=InnoDB`,
		`CREATE TABLE events (id INT NOT NULL PRIMARY KEY, kind VARCHAR(20)) ENGINE=InnoDB`,
		`INSERT INTO customers VALUES (1, 'one')`,
		`INSERT INTO invoices VALUES (5, 9.50)`,
		`INSERT INTO events VALUES (7, 'seven')`,
	} {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	src.Close()

	// A target that holds none of the names passes, and lists nothing.
	report, err := Preflight(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Existing objects"); c.Status != "ok" || !report.OK || len(report.Existing) != 0 {
		t.Fatalf("clean target: ok=%v, existing objects %s: %s, listed %v", report.OK, c.Status, c.Detail, report.Existing)
	}

	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })
	if _, err := dst.PgConn().Exec(ctx, `
		CREATE TABLE unrelated (name text);
		CREATE INDEX customers_name_idx ON unrelated (name);
		CREATE VIEW invoices_amount_idx AS SELECT 1 AS one;
		CREATE TYPE events AS ENUM ('other');
		CREATE TABLE customers (other int);
		INSERT INTO customers VALUES (42);
	`).ReadAll(); err != nil {
		t.Fatal(err)
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
const wantExisting = "index public.customers_name_idx; table public.customers; type public.events; view public.invoices_amount_idx"

// definitions describes the relations, indexes and types of the target's
// schema, and what the table that was there holds.
func definitions(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var s string
	if err := dst.QueryRow(ctx, `SELECT concat_ws(' | ',
		(SELECT string_agg(relname || ':' || relkind::text, ',' ORDER BY relname COLLATE "C") FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind <> 'i'),
		(SELECT string_agg(indexname || ' on ' || tablename, ',' ORDER BY indexname COLLATE "C") FROM pg_indexes WHERE schemaname = 'public'),
		(SELECT string_agg(viewname || ':' || md5(definition), ',') FROM pg_views WHERE schemaname = 'public'),
		(SELECT string_agg(t.typname || ':' || e.enumlabel, ',') FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid),
		(SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'customers'::regclass AND attnum > 0),
		(SELECT string_agg(c::text, ',') FROM customers c))`).Scan(&s); err != nil {
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
	if c := findCheck(t, report, "Target is empty"); c.Status != "fail" || !strings.Contains(c.Detail, "customers") {
		t.Errorf("target is empty: %s: %s", c.Status, c.Detail)
	}
	existing := findCheck(t, report, "Existing objects")
	if existing.Status != "fail" {
		t.Errorf("existing objects: %s", existing.Status)
	}
	for _, want := range []string{
		"tables: events (the name of a type)", "indexes: customers_name_idx, invoices_amount_idx (the name of a view)", "never replaces or merges",
	} {
		if !strings.Contains(existing.Detail, want) {
			t.Errorf("existing objects does not mention %q", want)
		}
	}

	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	out, err := Run(ctx, "mig_exist1", cfg, mymy.State{}, srcCfg, dstCfg, rep)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") ||
		!strings.Contains(err.Error(), "Existing objects") || !strings.Contains(err.Error(), "Target is empty") {
		t.Fatalf("run over existing objects: %v %v", out, err)
	}
	if after := definitions(t, ctx, dst); after != before {
		t.Errorf("the refused job changed the target:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMigrationExistingSkip keeps what the target has. The tables whose
// names are taken are left out of the migration, the index that would take
// a name in use is not created, and the rest arrives.
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
	out, err := Run(ctx, "mig_skip1", cfg, mymy.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	// Only the one table whose name was free is new, with its primary key.
	want := strings.Replace(before, "customers:r,", "customers:r,invoices:r,", 1)
	want = strings.Replace(want, "customers_name_idx on unrelated", "customers_name_idx on unrelated,invoices_pkey on invoices", 1)
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after keeping what was there:\nwant %s\ngot  %s", want, after)
	}
	_, st := rep.snapshot()
	sort.Strings(st.SkippedTables)
	sort.Strings(st.KeptObjects)
	if strings.Join(st.SkippedTables, ",") != "customers,events" ||
		strings.Join(st.KeptObjects, ",") != "index customers_name_idx,index invoices_amount_idx" || len(st.DropFirst) != 0 {
		t.Errorf("remembered: skipped %v, kept %v, drop %v", st.SkippedTables, st.KeptObjects, st.DropFirst)
	}
	if v, _ := rep.tables["invoices"]["validation"].(string); len(rep.tables) != 1 || !strings.HasSuffix(v, "match") {
		t.Errorf("tables copied and validated: %v", rep.tables)
	}
	var amount string
	if err := dst.QueryRow(ctx, `SELECT string_agg(id || ':' || amount, ',') FROM invoices`).Scan(&amount); err != nil || amount != "5:9.50" {
		t.Errorf("the table that was not there holds %q: %v", amount, err)
	}

	// A resumed job still knows what it left out, now that its own table
	// exists as well.
	out, err = Run(ctx, "mig_skip1", cfg, st, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("resumed run: %v %v", out, err)
	}
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("the resumed job changed the target:\nwant %s\ngot  %s", want, after)
	}
}

// TestMigrationExistingReplace drops what the target has under the
// migration's names and puts the source's tables and indexes in their place.
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
	for name, object := range map[string]string{"Target is empty": "customers", "Existing objects": "invoices_amount_idx (the name of a view)"} {
		c := findCheck(t, report, name)
		t.Logf("preflight %-26s %-4s %s", c.Name, c.Status, c.Detail)
		if c.Status != "warn" || !strings.Contains(c.Detail, "DROPPED") || !strings.Contains(c.Detail, object) {
			t.Errorf("%s: %s: %s", name, c.Status, c.Detail)
		}
	}

	var dropped bool
	rep := &eventReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(msg string) {
		dropped = dropped || strings.Contains(msg, "dropped 1 existing table(s) and 3 other object(s) on the target, as chosen")
	}}
	out, err := Run(ctx, "mig_replace1", cfg, mymy.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v %v", out, err)
	}
	if !dropped {
		t.Error("the job did not report what it dropped")
	}
	// The source's tables and indexes hold the names now; the table nobody
	// asked to replace is still there, without the index that was in the way.
	const want = "customers:r,events:r,invoices:r,unrelated:r | " +
		"customers_name_idx on customers,customers_pkey on customers,events_pkey on events,invoices_amount_idx on invoices,invoices_pkey on invoices | " +
		"id,name | (1,one)"
	if after := definitions(t, ctx, dst); after != want {
		t.Errorf("after replacing:\nwant %s\ngot  %s", want, after)
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
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_x_%d", time.Now().UnixNano())
	srcAdmin, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srcAdmin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	pgAdmin, err := postgres.Connect(ctx, dstAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgAdmin.Exec(ctx, "CREATE DATABASE "+db+" ENCODING 'UTF8' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	pgAdmin.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		srcAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		srcAdmin.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	dstCfg := dstAdmin
	srcCfg.Database, dstCfg.Database = db, db

	src, err := mysql.Open(ctx, srcCfg, map[string]string{"information_schema_stats_expiry": "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	setup, err := src.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range fixture {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	setup.Close()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)

	// Everything a job leaves behind: whatever the source database holds,
	// and on the target the relations and constraints of every schema that
	// is not the server's own, the checkpoint schema among them.
	state := func() (source, target string) {
		t.Helper()
		if err := src.QueryRowContext(ctx, `SELECT CONCAT_WS(' | ',
			(SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()),
			(SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()),
			(SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE()))`).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if err := dst.QueryRow(ctx, `SELECT concat_ws(' | ',
			(SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'),
			coalesce((SELECT string_agg(c.relname || ':' || c.relkind::text, ',' ORDER BY c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'), ''),
			coalesce((SELECT string_agg(conname || ':' || contype::text, ',' ORDER BY conname) FROM pg_constraint k JOIN pg_namespace n ON n.oid = k.connamespace
				WHERE k.contype IN ('p', 'u', 'c', 'f') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'), ''))`).Scan(&target); err != nil {
			t.Fatal(err)
		}
		return source, target
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

	cfg := migrate.Config{}
	list, err := pipeline{}.Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	unchanged("the preview")
	var lines []string
	for _, s := range list {
		lines = append(lines, previewLine(s))
	}
	// Index names are unique per table in MySQL and per schema here, so
	// each carries its table's name. Indexes and foreign keys are built
	// once the rows are in; the translated view comes at the very end.
	want := []string{
		"schema | table | customers", "schema | table | events", "schema | table | invoices",
		"indexing | index | customers_big_small", "indexing | index | customers_name_idx", "indexing | index | invoices_invoices_customer",
		"indexing | foreign key | invoices invoices_customer",
		"cutover | view | customer_totals",
	}
	if got := strings.Join(lines, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("preview:\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	checkPhaseOrder(t, list)
	for _, exact := range []struct{ kind, name, sql string }{
		{"table", "events", `CREATE TABLE "public"."events" (` + "\n" + `  "kind" varchar(20),` + "\n" + `  "payload" varchar(100),` + "\n" + `  "at" timestamp` + "\n)"},
		{"index", "customers_big_small", `CREATE UNIQUE INDEX "customers_big_small" ON "public"."customers" ("big", "small", "id")`},
		{"foreign key", "invoices invoices_customer",
			`ALTER TABLE "public"."invoices" ADD CONSTRAINT "invoices_customer" FOREIGN KEY ("customer_id") REFERENCES "public"."customers" ("id") ON UPDATE CASCADE ON DELETE CASCADE`},
	} {
		if s := findStatement(t, list, exact.kind, exact.name); s.SQL != exact.sql {
			t.Errorf("%s %s:\n got %s\nwant %s", exact.kind, exact.name, s.SQL, exact.sql)
		}
	}
	customers := findStatement(t, list, "table", "customers").SQL
	for _, part := range []string{`CREATE TABLE "public"."customers" (`, `"id" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL`, `"feeling" text DEFAULT 'ok'`,
		`CONSTRAINT "customers_feeling_enum" CHECK ("feeling" IN ('sad', 'ok', 'happy', ''))`, `"profile" jsonb`, `"big" numeric(20,0)`,
		`"created" timestamp(6)`, `"seen" timestamptz`, `PRIMARY KEY ("id")`} {
		if !strings.Contains(customers, part) {
			t.Errorf("CREATE TABLE of customers lacks %q:\n%s", part, customers)
		}
	}

	// A table-level job previews its tables only; the foreign key to a
	// table that is not migrated is left out.
	scoped, err := Preview(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline, Tables: []string{"invoices"}})
	if err != nil {
		t.Fatal(err)
	}
	lines = nil
	for _, s := range scoped {
		lines = append(lines, previewLine(s))
	}
	if got := strings.Join(lines, "; "); got != "schema | table | invoices; indexing | index | invoices_invoices_customer" {
		t.Errorf("table scope previews %s", got)
	}

	// What the target already has: a table of the migration, and a view
	// under the name one of its indexes would take. From here on the
	// previews depend on it.
	if _, err := dst.Exec(ctx, `CREATE TABLE customers (id int); INSERT INTO customers VALUES (42); CREATE VIEW customers_name_idx AS SELECT 1 AS one`); err != nil {
		t.Fatal(err)
	}
	_, dstBefore = state()

	// Under the default policy the job would refuse to start; its preview
	// still shows every object as it would be created.
	again, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(list) {
		t.Error("the default policy previews something else once the target holds some of the names")
	}

	// Kept: the table is left out with its indexes and the foreign key that
	// refers to it. Nothing is dropped.
	cfg.OnExisting = migrate.ExistingSkip
	skip, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	lines = nil
	for _, s := range skip {
		lines = append(lines, previewLine(s))
	}
	if got := strings.Join(lines, "; "); got != "schema | table | events; schema | table | invoices; indexing | index | invoices_invoices_customer" {
		t.Errorf("skip previews %s", got)
	}

	// Replaced: the drops come first, then the whole migration.
	cfg.OnExisting = migrate.ExistingReplace
	replace, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, replace)
	drops := []migrate.Statement{
		{Phase: migrate.PhaseSchema, Kind: "drop", Name: "public.customers", SQL: `DROP TABLE IF EXISTS public.customers CASCADE`},
		{Phase: migrate.PhaseSchema, Kind: "drop", Name: "public.customers_name_idx", SQL: `DROP VIEW IF EXISTS public.customers_name_idx CASCADE`},
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
	const built = "public | customer_totals:v,customers:r,customers_big_small:i,customers_id_seq:S,customers_name_idx:i,customers_pkey:i,events:r," +
		"invoices:r,invoices_id_seq:S,invoices_invoices_customer:i,invoices_pkey:i | " +
		"customers_feeling_enum:c,customers_pkey:p,invoices_customer:f,invoices_pkey:p"
	if _, got := state(); got != built {
		t.Errorf("the script built:\n got %s\nwant %s", got, built)
	}
}
