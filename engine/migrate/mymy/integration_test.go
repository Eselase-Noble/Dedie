package mymy

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

// The integration test needs two MySQL servers, the source with binary
// logging in ROW format. Point it at them with an account that may create
// databases:
//
//	DEDIE_TEST_MYSQL_SOURCE=mysql://root@127.0.0.1:33161
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
	return connector.Config{Host: u.Hostname(), Port: port, Username: u.User.Username(), Password: pw, SSLMode: "disable"}
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

// The fixture covers the types whose binary log form differs from their SQL
// form: unsigned integers past the signed range, ENUM and SET (logged as
// numbers), JSON, BIT, binary data, and text in a non-UTF-8 character set.
var fixture = []string{
	`CREATE TABLE customers (
		id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL,
		legacy_name VARCHAR(100) CHARACTER SET latin1, feeling ENUM('sad','ok','happy') DEFAULT 'ok',
		tags SET('a','b','c'), profile JSON, avatar BLOB, flags BIT(8), small TINYINT UNSIGNED,
		medium MEDIUMINT UNSIGNED, big BIGINT UNSIGNED, balance DECIMAL(12,2) DEFAULT 0, ratio DOUBLE,
		born DATE, wakes TIME, yr YEAR, created DATETIME(6), seen TIMESTAMP NULL, bio LONGTEXT,
		updated TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		name_upper VARCHAR(100) GENERATED ALWAYS AS (UPPER(name)) STORED, KEY name_idx (name)) ENGINE=InnoDB`,
	`CREATE TABLE invoices (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, customer_id INT UNSIGNED NOT NULL, amount DECIMAL(12,2) NOT NULL,
		CONSTRAINT invoices_customer FOREIGN KEY (customer_id) REFERENCES customers (id) ON UPDATE CASCADE ON DELETE CASCADE) ENGINE=InnoDB`,
	`CREATE TABLE events (kind VARCHAR(20), payload VARCHAR(100)) ENGINE=InnoDB`,
	`CREATE TABLE change_log (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, note VARCHAR(100)) ENGINE=InnoDB`,
	`CREATE TABLE scratch (v INT) ENGINE=InnoDB`,
	`CREATE TABLE legacy_counters (name VARCHAR(20) PRIMARY KEY, n INT) ENGINE=MyISAM`,
	`CREATE TRIGGER customers_log AFTER INSERT ON customers FOR EACH ROW INSERT INTO change_log (note) VALUES (CONCAT('insert ', NEW.id))`,
	`CREATE VIEW customer_totals AS SELECT c.id, c.name, COALESCE(SUM(i.amount), 0) AS total FROM customers c LEFT JOIN invoices i ON i.customer_id = c.id GROUP BY c.id, c.name`,
	`CREATE PROCEDURE touch(IN cid INT) UPDATE customers SET seen = NOW() WHERE id = cid`,
	`SET SESSION cte_max_recursion_depth = 100000`,
	`INSERT INTO customers (name, legacy_name, feeling, tags, profile, avatar, flags, small, medium, big, balance, ratio, born, wakes, yr, created, seen, bio)
		WITH RECURSIVE seq (g) AS (SELECT 1 UNION ALL SELECT g + 1 FROM seq WHERE g < 5000)
		SELECT CONCAT('user ', g, ' ☃'), CONCAT('caf', CHAR(233 USING latin1), ' ', g), ELT(1 + g % 3, 'sad', 'ok', 'happy'), 'a,c',
			JSON_OBJECT('n', g, 'tags', JSON_ARRAY('x', 'y')), UNHEX(MD5(g)), b'10100101', 250, 16000000, 18446744073709551000 + g % 600,
			g * 1.25, g / 7, '1990-05-17', '06:30:00', 2024, '2024-02-29 13:14:15.123456', '2024-02-29 13:14:15', REPEAT(MD5(g), 100)
		FROM seq`,
	`INSERT INTO invoices (customer_id, amount) SELECT 1 + (id * 7) % 5000, id * 1.5 FROM customers`,
	`INSERT INTO invoices (customer_id, amount) SELECT 1 + (id * 13) % 5000, id * 2.5 FROM customers`,
	`INSERT INTO events SELECT CONCAT('k', id % 10), CONCAT('payload ', id) FROM customers WHERE id <= 1000`,
}

func TestMigration(t *testing.T) {
	srcCfg, dstCfg := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := fmt.Sprintf("dedie_test_%d", time.Now().UnixNano())
	for _, admin := range []connector.Config{srcCfg, dstCfg} {
		c, err := mysql.Open(ctx, admin, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c.Exec("ALTER DATABASE " + db + " READ ONLY = 0")
			c.Exec("DROP DATABASE IF EXISTS " + db)
			c.Close()
		})
	}
	srcCfg.Database, dstCfg.Database = db, db

	// Read current figures from information_schema, not cached statistics.
	fresh := map[string]string{"information_schema_stats_expiry": "0"}
	src, err := mysql.Open(ctx, srcCfg, fresh)
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

	// Table-level scope: only the named tables with their triggers, no
	// views or routines, and a clear report of names that do not exist.
	scoped, err := BuildPlan(ctx, src, db, migrate.Config{Tables: []string{"customers", db + ".invoices", "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Tables) != 2 || len(scoped.Missing) != 1 || scoped.Missing[0] != "nope" || len(scoped.Post) != 0 || len(scoped.Final) != 1 {
		t.Fatalf("table scope: %d tables, missing %v, %d post, %d final", len(scoped.Tables), scoped.Missing, len(scoped.Post), len(scoped.Final))
	}

	cfg := migrate.Config{ExcludeTables: []string{"scratch"}, FenceSource: true, CutoverTimeoutSeconds: 60}
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
		for i := 0; i < 200; i++ {
			n := round*1000 + i
			stmts := []string{
				fmt.Sprintf(`INSERT INTO customers (name, feeling, tags, profile, big, small, bio) VALUES ('live %d', 'happy', 'b', '{"live": true}', 18446744073709551615, 255, REPEAT('x', 5000))`, n),
				fmt.Sprintf(`UPDATE customers SET balance = balance + 1, feeling = 'sad', tags = 'a,b,c', legacy_name = NULL WHERE id = %d`, i+1),
				fmt.Sprintf(`INSERT INTO invoices (customer_id, amount) VALUES (%d, 9.99)`, i+1),
				fmt.Sprintf(`DELETE FROM invoices WHERE id = %d`, n%10000+1),
				fmt.Sprintf(`UPDATE events SET payload = 'changed %d' WHERE payload = 'payload %d'`, n, i+1+round*200),
				// Changing or removing a parent cascades to its invoices on
				// the source without logging them; the target must follow.
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
	run := func(st State) (migrate.Outcome, error) { return Run(ctx, "mig_test1", cfg, st, srcCfg, dstCfg, rep) }
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
		out migrate.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() { o, e := run(State{}); done <- result{o, e} }()
	go workload(0)
	waitFor("streaming", func(p string, st State) bool { return p == migrate.PhaseStreaming })

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
	if _, err := src.ExecContext(ctx, `INSERT INTO events VALUES ('after', 'truncate')`); err != nil {
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

	dst, err := mysql.Open(ctx, dstCfg, fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	check := func(query string) {
		t.Helper()
		var a, b sql.NullString
		if err := src.QueryRowContext(ctx, query).Scan(&a); err != nil {
			t.Fatalf("%s on source: %v", query, err)
		}
		if err := dst.QueryRowContext(ctx, query).Scan(&b); err != nil {
			t.Fatalf("%s on target: %v", query, err)
		}
		if a != b {
			t.Errorf("%s: source %s, target %s", query, a.String, b.String)
		} else {
			t.Logf("ok  %-78s %s", query, a.String)
		}
	}
	check(`SELECT COUNT(*) FROM customers`)
	check(`SELECT COUNT(*) FROM invoices`)
	check(`SELECT BIT_XOR(CRC32(CONCAT_WS('|', id, name, IFNULL(HEX(legacy_name), '~'), feeling, IFNULL(tags, '~'), profile, IFNULL(HEX(avatar), '~'),
		IFNULL(HEX(flags), '~'), IFNULL(small, '~'), IFNULL(medium, '~'), IFNULL(big, '~'), balance, IFNULL(ratio, '~'), IFNULL(born, '~'),
		IFNULL(wakes, '~'), IFNULL(yr, '~'), IFNULL(created, '~'), IFNULL(seen, '~'), MD5(bio), name_upper, updated))) FROM customers`)
	check(`SELECT BIT_XOR(CRC32(CONCAT_WS('|', id, customer_id, amount))) FROM invoices`)
	check(`SELECT BIT_XOR(CRC32(CONCAT_WS('|', kind, payload))) FROM events`)
	check(`SELECT MAX(big) FROM customers`)
	check(`SELECT HEX(legacy_name) FROM customers WHERE id = 2500`)
	// The trigger must not have fired again on the target for replicated rows.
	check(`SELECT COUNT(*) FROM change_log`)
	check(`SELECT auto_increment FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'customers'`)
	check(`SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema = DATABASE()`)
	check(`SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema = DATABASE()`)
	check(`SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema = DATABASE() AND constraint_type = 'FOREIGN KEY'`)
	check(`SELECT SUM(total) FROM customer_totals`)

	if _, err := dst.ExecContext(ctx, `INSERT INTO customers (name) VALUES ('post cutover')`); err != nil {
		t.Errorf("insert on target after cutover: %v", err)
	}
	var leftover int
	dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (?, 'scratch', 'legacy_counters')`, checkpointTable).Scan(&leftover)
	if leftover != 0 {
		t.Errorf("%d table(s) on the target that should not be there", leftover)
	}
	// The fence must hold.
	if _, err := src.ExecContext(ctx, `INSERT INTO change_log (note) VALUES ('late write')`); err == nil {
		t.Error("source accepted a write after a fenced cutover")
	}
	for name, tbl := range rep.tables {
		if tbl["validation"] != "match" {
			t.Errorf("table %s validation = %v", name, tbl["validation"])
		}
	}
	_, st := rep.snapshot()
	t.Logf("applied %d transactions, %d row changes, %d anomalies", st.TxApplied, st.RowsApplied, st.Anomalies)
	if st.Anomalies != 0 {
		t.Errorf("%d apply anomalies", st.Anomalies)
	}
}
