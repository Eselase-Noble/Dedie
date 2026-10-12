package orasrc

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
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// The test needs an Oracle schema it may create DEDIE_FW_ tables in:
//
//	DEDIE_TEST_ORACLE_SOURCE=oracle://dedie_test:secret@127.0.0.1:1521/FREEPDB1
//
// with what following changes needs; see the tests of package orapg.

// memTarget is a target that keeps its rows in memory: per table, the
// printed row by its key.
type memTarget struct {
	mu         sync.Mutex
	rows       map[string]map[string]string
	checkpoint int64
}

func (m *memTarget) Checkpoint(context.Context) (int64, error) { return m.checkpoint, nil }

func (m *memTarget) Apply(ctx context.Context, ops []RowOp, fetch Fetch, checkpoint int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range ops {
		label := op.Table.Label()
		if op.Truncate {
			m.rows[label] = map[string]string{}
			continue
		}
		row, err := fetch(ctx, op.Table, op.Key)
		if err != nil {
			return err
		}
		id := strings.Join(op.Key, "|")
		if row == nil {
			delete(m.rows[label], id)
		} else {
			m.rows[label][id] = fmt.Sprint(row...)
		}
	}
	m.checkpoint = checkpoint
	return nil
}

func (m *memTarget) table(label string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, row := range m.rows[label] {
		out = append(out, row)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

type quietReporter struct {
	mu      sync.Mutex
	t       *testing.T
	desired string
	events  []string
}

func (q *quietReporter) Event(level, msg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.t.Logf("[%s] %s", level, msg)
	q.events = append(q.events, msg)
}
func (q *quietReporter) SetPhase(string)                           {}
func (q *quietReporter) SaveState(any)                             {}
func (q *quietReporter) ResetTables([]migrate.TableProgress)       {}
func (q *quietReporter) UpdateTable(_, _ string, _ map[string]any) {}
func (q *quietReporter) Desired() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.desired
}
func (q *quietReporter) SetDesired(d string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.desired = d
}
func (q *quietReporter) said(text string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.events {
		if strings.Contains(e, text) {
			return true
		}
	}
	return false
}

// TestFollow follows a source in use both ways: with LogMiner where it
// can, and through row versions alone, as a job does where LogMiner is
// not available or has lost its redo. Either way the target must end up
// with exactly the source's rows.
func TestFollow(t *testing.T) {
	raw := os.Getenv("DEDIE_TEST_ORACLE_SOURCE")
	if raw == "" {
		t.Skip("DEDIE_TEST_ORACLE_SOURCE not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	src, err := oracle.Open(ctx, connector.Config{Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		Username: u.User.Username(), Password: pw, SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_, version, _ := oracle.Version(ctx, src)
	modern := oracle.VersionNum(version) >= 230000

	for _, mode := range []string{"redo where possible", "row versions only", "redo lost midway"} {
		t.Run(mode, func(t *testing.T) { follow(t, ctx, src, mode, modern) })
	}
}

func follow(t *testing.T, ctx context.Context, src *sql.DB, mode string, modern bool) {
	exec := func(db interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}, stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%v\n%s", err, stmt)
			}
		}
	}
	drop := func() {
		for _, name := range []string{"dedie_fw_plain", "dedie_fw_pair", "dedie_fw_modern", "dedie_fw_keyless"} {
			src.ExecContext(context.Background(), "DROP TABLE "+name+" PURGE")
		}
	}
	drop()
	t.Cleanup(drop)
	tables := []string{"DEDIE_FW_PLAIN", "DEDIE_FW_PAIR", "DEDIE_FW_KEYLESS"}
	exec(src,
		`CREATE TABLE dedie_fw_plain (id NUMBER PRIMARY KEY, note VARCHAR2(40), big CLOB)`,
		`CREATE TABLE dedie_fw_pair (day DATE, tag RAW(4), code CHAR(3), n NUMBER, PRIMARY KEY (day, tag, code))`,
		`CREATE TABLE dedie_fw_keyless (note VARCHAR2(40))`,
		`INSERT INTO dedie_fw_plain SELECT LEVEL, 'row ' || LEVEL, RPAD('x', 3000, 'x') FROM dual CONNECT BY LEVEL <= 500`,
		`UPDATE dedie_fw_plain SET big = big || RPAD('y', 4000, 'y') || RPAD('y', 4000, 'y') WHERE id = 5`,
		`INSERT INTO dedie_fw_pair SELECT DATE '2024-01-01' + LEVEL, HEXTORAW('00FF' || TO_CHAR(LEVEL, 'FM0X')), 'a', LEVEL FROM dual CONNECT BY LEVEL <= 50`)
	if modern {
		tables = append(tables, "DEDIE_FW_MODERN")
		exec(src, `CREATE TABLE dedie_fw_modern (id NUMBER(9) PRIMARY KEY, doc JSON, ok BOOLEAN)`,
			`INSERT INTO dedie_fw_modern SELECT LEVEL, JSON('{"n": 1}'), TRUE FROM dual CONNECT BY LEVEL <= 20`)
	}

	cat, err := LoadCatalog(ctx, src, migrate.Config{Tables: tables})
	if err != nil {
		t.Fatal(err)
	}
	// As a job does: key logging where LogMiner will be used, and none
	// where it will not.
	versionsOnly := mode == "row versions only"
	var logged []string
	if !versionsOnly {
		if err := LogKeys(ctx, src, cat, func(table string) { logged = append(logged, table) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range logged {
		if strings.HasSuffix(label, "MODERN") {
			t.Errorf("key logging was switched on for %s, which LogMiner does not follow", label)
		}
	}

	// A transaction open across the copy.
	early, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Rollback()
	exec(early, `UPDATE dedie_fw_plain SET note = 'open across the copy' WHERE id = 10`)

	snapshot, mineFrom, err := StartPosition(ctx, src, cat)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Pinned(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	target := &memTarget{rows: map[string]map[string]string{}}
	for _, tb := range cat.Tables {
		target.rows[tb.Label()] = map[string]string{}
		if len(tb.PK) == 0 {
			continue
		}
		list, err := keys(ctx, reader, tb, fmt.Sprintf("SELECT %s FROM %s AS OF SCN %d", keyList(tb), tb.Source(), snapshot))
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range list {
			row, err := FetchRow(ctx, reader, tb, key, snapshot)
			if err != nil || row == nil {
				t.Fatalf("copy %s %v: %v", tb.Label(), key, err)
			}
			target.rows[tb.Label()][strings.Join(key, "|")] = fmt.Sprint(row...)
		}
	}

	rep := &quietReporter{t: t, desired: "run"}
	f := &Follower{Source: src, Catalog: cat, Target: target, Reporter: rep,
		Advanced: func(int64, int64) {}, Applied: func(int64, int64) {}, Save: func() {},
		Anomaly: func(msg string) { rep.Event("anomaly", msg) }}
	if versionsOnly {
		f.useVersions(0, "the test has it so")
	}
	type result struct {
		reason string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		reason, err := f.Follow(ctx, mineFrom)
		done <- result{reason, err}
	}()

	if err := early.Commit(); err != nil {
		t.Fatal(err)
	}
	exec(src,
		`INSERT INTO dedie_fw_plain VALUES (1001, 'new', 'c')`,
		`UPDATE dedie_fw_plain SET note = 'changed' WHERE id BETWEEN 20 AND 60`,
		`DELETE FROM dedie_fw_plain WHERE id BETWEEN 100 AND 120`,
		`UPDATE dedie_fw_plain SET id = id + 5000 WHERE id BETWEEN 200 AND 210`,
		`DECLARE l CLOB; BEGIN SELECT big INTO l FROM dedie_fw_plain WHERE id = 5 FOR UPDATE; DBMS_LOB.WRITEAPPEND(l, 3, 'zzz'); COMMIT; END;`,
		`UPDATE dedie_fw_pair SET n = n * 2 WHERE n <= 10`,
		`UPDATE dedie_fw_pair SET code = 'b' WHERE n BETWEEN 30 AND 35`,
		`DELETE FROM dedie_fw_pair WHERE n > 45`,
		`INSERT INTO dedie_fw_pair VALUES (DATE '1999-12-31', HEXTORAW('DEADBEEF'), 'zz', 0)`,
		`INSERT INTO dedie_fw_keyless VALUES ('not followed')`,
		`BEGIN
			UPDATE dedie_fw_plain SET note = 'kept' WHERE id = 300;
			SAVEPOINT sp;
			DELETE FROM dedie_fw_plain WHERE id = 301;
			ROLLBACK TO sp;
			COMMIT;
		END;`)
	if modern {
		exec(src,
			`INSERT INTO dedie_fw_modern VALUES (100, JSON('{"new": true}'), FALSE)`,
			`UPDATE dedie_fw_modern SET doc = JSON('{"n": 2}') WHERE id <= 5`,
			`UPDATE dedie_fw_modern SET ok = FALSE WHERE id = 6`,
			`UPDATE dedie_fw_modern SET id = 207 WHERE id = 7`,
			`DELETE FROM dedie_fw_modern WHERE id >= 18 AND id < 100`)
	}
	undone, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec(undone, `DELETE FROM dedie_fw_plain WHERE id < 50`)
	undone.Rollback()

	if mode == "redo lost midway" {
		// What a log switch does to a job on a source that keeps no
		// archived redo: LogMiner is of no more use from one pass to the
		// next. The job is stopped, and started again believing so.
		time.Sleep(3 * time.Second)
		rep.SetDesired("pause")
		if r := <-done; r.err != nil || r.reason != StopPause {
			t.Fatalf("pause: %v, %v", r.reason, r.err)
		}
		exec(src, `UPDATE dedie_fw_plain SET note = 'while away' WHERE id = 400`, `DELETE FROM dedie_fw_plain WHERE id = 401`)
		rep.SetDesired("run")
		f.useVersions(target.checkpoint, "the test says the redo is gone")
		go func() {
			reason, err := f.Follow(ctx, mineFrom)
			done <- result{reason, err}
		}()
	}

	// A table emptied and filled again while it is followed.
	time.Sleep(2 * time.Second)
	exec(src, `TRUNCATE TABLE dedie_fw_pair`,
		`INSERT INTO dedie_fw_pair VALUES (DATE '2030-01-01', HEXTORAW('01'), 'new', 1)`)
	if modern {
		exec(src, `TRUNCATE TABLE dedie_fw_modern`, `INSERT INTO dedie_fw_modern VALUES (1, JSON('[1]'), NULL)`)
	}
	time.Sleep(8 * time.Second)
	exec(src, `INSERT INTO dedie_fw_pair VALUES (DATE '2030-01-02', HEXTORAW('02'), 'new', 2)`,
		`UPDATE dedie_fw_plain SET note = 'last' WHERE id = 1`)

	rep.SetDesired("cutover")
	select {
	case r := <-done:
		if r.err != nil || r.reason != StopCutover {
			t.Fatalf("follow: %v, %v", r.reason, r.err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the stream to drain")
	}

	for _, tb := range cat.Tables {
		if len(tb.PK) == 0 {
			continue
		}
		rows, err := reader.QueryContext(ctx, "SELECT "+SelectList(tb)+" FROM "+tb.Source())
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			values := make([]any, len(tb.Cols))
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			want = append(want, fmt.Sprint(values...))
		}
		rows.Close()
		sort.Strings(want)
		if got := target.table(tb.Label()); got != strings.Join(want, "\n") {
			t.Errorf("%s differs: the source has %d row(s), the target %d", tb.Label(), len(want), len(target.rows[tb.Label()]))
		} else {
			t.Logf("%s: %d row(s) match", tb.Label(), len(want))
		}
	}
	if rep.said("could not describe") {
		t.Error("a change LogMiner could not describe was reported instead of followed")
	}
	if got := rep.said("reading row versions"); got != (mode != "redo where possible") {
		t.Errorf("said it follows row versions: %v", got)
	}
	if !rep.said("DEDIE_FW_KEYLESS has no primary key") {
		t.Error("the table without a primary key was not reported")
	}
	if left, err := UnlogKeys(ctx, src, logged); err != nil || len(left) > 0 {
		t.Errorf("switch key logging off: %v, %v", left, err)
	}
}
