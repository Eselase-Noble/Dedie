package mypg

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
		if p, st := rep.snapshot(); p == migrate.PhaseStreaming && st.TxApplied > 0 {
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
