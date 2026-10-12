package oraora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
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

// The integration tests need two Oracle accounts, which may be on the
// same server:
//
//	DEDIE_TEST_ORACLE_SOURCE_C=oracle://dedie_test_c:secret@127.0.0.1:1521/FREEPDB1
//	DEDIE_TEST_ORACLE_TARGET=oracle://dedie_test_d:secret@127.0.0.1:1521/FREEPDB1
//
// The source account's own schema is migrated whole into the target
// account's own schema, so both should be kept for this purpose; the
// tests create and drop the DEDIE_IT_ objects in them. Each account
// needs CREATE SESSION, TABLE, SEQUENCE, VIEW, PROCEDURE, TRIGGER,
// SYNONYM and TYPE and a quota on its tablespace. The online test also
// needs what following changes needs: minimal supplemental logging on the
// database, and LOGMINING, SELECT ANY TRANSACTION, EXECUTE_CATALOG_ROLE
// and SELECT ANY DICTIONARY for the source account.

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
	events  []string
}

func (m *memReporter) Event(level, msg string) {
	m.t.Logf("[%s] %s", level, msg)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, level+": "+msg)
}
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

// said reports whether an event containing the text was reported.
func (m *memReporter) said(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if strings.Contains(e, text) {
			return true
		}
	}
	return false
}

func newReporter(t *testing.T) *memReporter {
	return &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
}

// drops removes everything the tests create, in either schema; errors
// for objects that are not there are ignored.
var drops = []string{
	`DROP TRIGGER dedie_it_audit`,
	`DROP TRIGGER dedie_it_t`,
	`DROP VIEW dedie_it_totals`,
	`DROP VIEW dedie_it_v`,
	`DROP SYNONYM dedie_it_syn`,
	`DROP PACKAGE dedie_it_pkg`,
	`DROP FUNCTION dedie_it_twice`,
	`DROP FUNCTION dedie_it_f`,
	`DROP TABLE dedie_it_invoices CASCADE CONSTRAINTS PURGE`,
	`DROP TABLE dedie_it_customers CASCADE CONSTRAINTS PURGE`,
	`DROP TABLE dedie_it_events PURGE`,
	`DROP TABLE "Dedie_It_Mixed" PURGE`,
	`DROP TABLE dedie_it_modern PURGE`,
	`DROP TABLE dedie_it_parts PURGE`,
	`DROP TABLE dedie_it_other CASCADE CONSTRAINTS PURGE`,
	`DROP TABLE dedie_it_keep CASCADE CONSTRAINTS PURGE`,
	`DROP TABLE dedie_checkpoint PURGE`,
	`DROP SEQUENCE dedie_it_seq`,
	`DROP SEQUENCE dedie_it_s`,
}

// The fixture covers every family of types: the NUMBER family, dates and
// timestamps with and without zones, intervals, large objects (among
// them one that is empty rather than NULL), raw bytes, fixed-width and
// national text. Around them it has what a same-engine migration carries
// over as it is: an identity column that is GENERATED ALWAYS, a virtual
// column, defaults that call a sequence and a function, unique and check
// constraints, a function-based index, a foreign key that cascades, a
// partitioned table, a table without a primary key, a name written in
// mixed case, a view, a synonym, stored code, a trigger and comments.
var fixture = []string{
	`CREATE SEQUENCE dedie_it_seq START WITH 100 INCREMENT BY 5`,
	`CREATE TABLE dedie_it_customers (
		id NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, code VARCHAR2(20) NOT NULL, name NVARCHAR2(100),
		status CHAR(1) DEFAULT 'A' NOT NULL, small NUMBER(4), medium NUMBER(9), big NUMBER(18), huge NUMBER(30),
		balance NUMBER(12,2) DEFAULT 0, ratio BINARY_DOUBLE, single BINARY_FLOAT, flt FLOAT, wide NUMBER, born DATE DEFAULT SYSDATE, created TIMESTAMP(9),
		seen TIMESTAMP WITH TIME ZONE, local_seen TIMESTAMP WITH LOCAL TIME ZONE, tenure INTERVAL YEAR TO MONTH,
		shift INTERVAL DAY TO SECOND, bio CLOB, notes NCLOB, avatar BLOB, token RAW(16) DEFAULT SYS_GUID(),
		ticket NUMBER DEFAULT dedie_it_seq.NEXTVAL, code_upper VARCHAR2(20) GENERATED ALWAYS AS (UPPER(code)) VIRTUAL,
		CONSTRAINT dedie_it_cust_code UNIQUE (code), CONSTRAINT dedie_it_cust_bal CHECK (balance >= 0))`,
	`CREATE INDEX dedie_it_cust_nums ON dedie_it_customers (medium, small)`,
	`CREATE INDEX dedie_it_cust_fn ON dedie_it_customers (LOWER(code))`,
	`COMMENT ON TABLE dedie_it_customers IS 'who we sell to; it''s the main table'`,
	`COMMENT ON COLUMN dedie_it_customers.code IS 'short name'`,
	`CREATE TABLE dedie_it_invoices (
		id NUMBER(18) PRIMARY KEY, customer_id NUMBER NOT NULL, amount NUMBER(12,2) NOT NULL, line VARCHAR2(10),
		CONSTRAINT dedie_it_inv_cust FOREIGN KEY (customer_id) REFERENCES dedie_it_customers (id) ON DELETE CASCADE)`,
	`CREATE TABLE dedie_it_events (kind VARCHAR2(20), payload VARCHAR2(100), at DATE)`,
	`CREATE TABLE "Dedie_It_Mixed" ("Id" NUMBER(9) PRIMARY KEY, "Note" VARCHAR2(50))`,
	`CREATE TABLE dedie_it_parts (id NUMBER(9) PRIMARY KEY, region VARCHAR2(10))
		PARTITION BY RANGE (id) (PARTITION p_low VALUES LESS THAN (100), PARTITION p_high VALUES LESS THAN (MAXVALUE)) ENABLE ROW MOVEMENT`,
	`CREATE VIEW dedie_it_totals AS SELECT customer_id, SUM(amount) total FROM dedie_it_invoices GROUP BY customer_id`,
	`CREATE SYNONYM dedie_it_syn FOR dedie_it_invoices`,
	`CREATE FUNCTION dedie_it_twice (n NUMBER) RETURN NUMBER IS BEGIN RETURN n * 2; END;`,
	`CREATE PACKAGE dedie_it_pkg AS FUNCTION invoices RETURN NUMBER; END;`,
	`CREATE PACKAGE BODY dedie_it_pkg AS
		FUNCTION invoices RETURN NUMBER IS n NUMBER; BEGIN SELECT COUNT(*) INTO n FROM dedie_it_invoices; RETURN n; END;
	END;`,
	`INSERT INTO dedie_it_customers (code, name, small, medium, big, huge, balance, ratio, single, flt, wide, born, created, seen, local_seen, tenure, shift, bio, notes, avatar)
		SELECT 'c' || LEVEL, N'user ' || LEVEL || UNISTR(' \2603'), MOD(LEVEL, 9999), LEVEL * 1000, LEVEL * 1000000000000,
			123456789012345678901234567890 - LEVEL, LEVEL * 1.25, LEVEL / 7, LEVEL / 7, LEVEL / 3, LEVEL / 3,
			DATE '1990-05-17' + LEVEL, TIMESTAMP '2024-02-29 13:14:15.123456789', TIMESTAMP '2024-02-29 13:14:15.5 +05:30',
			TIMESTAMP '2024-02-29 13:14:15', INTERVAL '2-6' YEAR TO MONTH, INTERVAL '-1 02:03:04.5' DAY TO SECOND,
			'tab' || CHR(9) || 'here, backslash \ and' || CHR(10) || 'newline ' || RPAD('x', CASE WHEN MOD(LEVEL, 25) IN (0, 7) THEN 3000 ELSE 30 END, 'x'), TO_NCLOB(UNISTR('n\00e9 \2603 ')) || LEVEL,
			HEXTORAW('00FF10') FROM dual CONNECT BY LEVEL <= 3000`,
	`UPDATE dedie_it_customers SET bio = bio || RPAD('y', 4000, 'y') || RPAD('z', 4000, 'z'), small = NULL, name = NULL WHERE MOD(id, 100) = 0`,
	// The extremes: a number too long to print in full, the largest and
	// smallest magnitudes, values that are not numbers, an empty large
	// object next to a NULL one, a date long ago.
	`UPDATE dedie_it_customers SET wide = 1.23456789012345678901234567890123456789E+125, ratio = BINARY_DOUBLE_INFINITY, single = BINARY_FLOAT_NAN,
		huge = -99999999999999999999999999999, bio = EMPTY_CLOB(), notes = NULL, avatar = EMPTY_BLOB(), born = DATE '0001-01-01',
		seen = TIMESTAMP '1999-12-31 23:59:59.999999 -11:00', tenure = INTERVAL '-99-11' YEAR TO MONTH WHERE id = 11`,
	`UPDATE dedie_it_customers SET wide = -1.5E-130, bio = NULL, notes = EMPTY_CLOB(), avatar = NULL, token = NULL WHERE id = 12`,
	`INSERT INTO dedie_it_invoices SELECT id, 1 + MOD(id * 7, 3000), id * 1.5, 'l' || id FROM dedie_it_customers`,
	`INSERT INTO dedie_it_events SELECT 'k' || MOD(id, 10), 'payload ' || id, DATE '2024-01-01' FROM dedie_it_customers WHERE id <= 1000`,
	`INSERT INTO "Dedie_It_Mixed" VALUES (1, 'kept as written')`,
	`INSERT INTO dedie_it_parts SELECT LEVEL, 'r' || MOD(LEVEL, 4) FROM dual CONNECT BY LEVEL <= 300`,
	// The trigger writes to a table that is only copied. Had it fired on
	// the target for a copied or replicated row, that table would have
	// more rows there than on the source.
	`CREATE TRIGGER dedie_it_audit AFTER INSERT ON dedie_it_invoices FOR EACH ROW
		BEGIN INSERT INTO dedie_it_events VALUES ('audit', 'invoice ' || :NEW.id, DATE '2024-06-01'); END;`,
}

func open(t *testing.T, ctx context.Context, cfg connector.Config) *sql.DB {
	db, err := oracle.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// setup empties both schemas of the tests' objects, now and when the
// test ends, and runs the given statements on the source.
func setup(t *testing.T, ctx context.Context, statements []string) (srcCfg, dstCfg connector.Config, src, dst *sql.DB) {
	srcCfg, dstCfg = testConfig(t, "DEDIE_TEST_ORACLE_SOURCE_C"), testConfig(t, "DEDIE_TEST_ORACLE_TARGET")
	src, dst = open(t, ctx, srcCfg), open(t, ctx, dstCfg)
	clean := func() {
		bg := context.Background()
		for _, db := range []*sql.DB{src, dst} {
			for _, stmt := range drops {
				db.ExecContext(bg, stmt)
			}
		}
	}
	t.Cleanup(clean)
	clean()
	mustExec(t, ctx, src, statements...)
	return srcCfg, dstCfg, src, dst
}

// fullFixture is the fixture with what only a current release has.
func fullFixture(t *testing.T, ctx context.Context) []string {
	list := fixture
	src := open(t, ctx, testConfig(t, "DEDIE_TEST_ORACLE_SOURCE_C"))
	_, version, _ := oracle.Version(ctx, src)
	if oracle.VersionNum(version) >= 230000 {
		list = append(list[:len(list):len(list)],
			`CREATE TABLE dedie_it_modern (id NUMBER(9) PRIMARY KEY, doc JSON, ok BOOLEAN)`,
			`INSERT INTO dedie_it_modern VALUES (1, JSON('{"a": [1, 2], "b": "x"}'), TRUE)`,
			`INSERT INTO dedie_it_modern VALUES (2, NULL, FALSE)`)
	}
	return list
}

func mustExec(t *testing.T, ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
}

// one runs a query that returns a single value and gives it back as text.
func one(ctx context.Context, db *sql.DB, query string) (string, error) {
	var v sql.NullString
	err := db.QueryRowContext(ctx, query).Scan(&v)
	return v.String, err
}

func mustOne(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	v, err := one(ctx, db, query)
	if err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return v
}

// rowsOf reads a whole table as one line of text per row, sorted. Every
// value is read in a form that leaves nothing out: scalar values as the
// bytes Oracle stores them in (DUMP), so that two numbers or timestamps
// are equal only if they are the same to the last digit and with the
// same time zone; large objects in full, reduced to a digest here, with
// NULL and empty told apart.
func rowsOf(t *testing.T, ctx context.Context, db *sql.DB, table string) []string {
	t.Helper()
	bare := strings.Trim(table, `"`)
	if bare == table {
		bare = strings.ToUpper(table)
	}
	cols, err := queryText(ctx, db, `SELECT column_name, data_type FROM user_tab_cols WHERE table_name = :1 AND hidden_column = 'NO' ORDER BY column_id`, bare)
	if err != nil || len(cols) == 0 {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var exprs []string
	large := map[int]bool{}
	for _, c := range cols {
		col := oracle.Quote(c[0])
		switch c[1] {
		case "CLOB", "NCLOB", "BLOB":
			large[len(exprs)] = true
			exprs = append(exprs, col, "CASE WHEN "+col+" IS NULL THEN 'null' WHEN DBMS_LOB.GETLENGTH("+col+") = 0 THEN 'empty' ELSE 'value' END")
		case "JSON":
			exprs = append(exprs, "JSON_SERIALIZE("+col+" RETURNING VARCHAR2(4000))")
		case "BOOLEAN":
			exprs = append(exprs, "TO_CHAR("+col+")")
		default:
			exprs = append(exprs, "DUMP("+col+", 16)")
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values := make([]any, len(exprs))
		dest := make([]any, len(exprs))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(values))
		for i, v := range values {
			switch x := v.(type) {
			case nil:
				parts[i] = "NULL"
			case []byte:
				parts[i] = fmt.Sprintf("%d:%x", len(x), sha256.Sum256(x))
			case string:
				parts[i] = x
				if large[i] {
					parts[i] = fmt.Sprintf("%d:%x", len(x), sha256.Sum256([]byte(x)))
				}
			default:
				parts[i] = fmt.Sprint(x)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// compare checks that a table holds exactly the same rows on both sides.
func compare(t *testing.T, ctx context.Context, src, dst *sql.DB, tables ...string) {
	t.Helper()
	for _, table := range tables {
		a, b := rowsOf(t, ctx, src, table), rowsOf(t, ctx, dst, table)
		if len(a) != len(b) {
			t.Errorf("%s: %d rows on the source, %d on the target", table, len(a), len(b))
			continue
		}
		differ := 0
		for i := range a {
			if a[i] != b[i] {
				if differ == 0 {
					t.Errorf("%s: rows differ, the first:\nsource %s\ntarget %s", table, a[i], b[i])
				}
				differ++
			}
		}
		if differ > 0 {
			t.Errorf("%s: %d of %d rows differ", table, differ, len(a))
		}
	}
}

func fixtureTables(ctx context.Context, src *sql.DB) []string {
	tables := []string{"dedie_it_customers", "dedie_it_invoices", "dedie_it_events", `"Dedie_It_Mixed"`, "dedie_it_parts"}
	if n, _ := one(ctx, src, `SELECT COUNT(*) FROM user_tables WHERE table_name = 'DEDIE_IT_MODERN'`); n == "1" {
		tables = append(tables, "dedie_it_modern")
	}
	return tables
}

// checkValues looks at a few values by eye, as a guard on the comparison
// itself: these are what the rows must hold, on the target.
func checkValues(t *testing.T, ctx context.Context, dst *sql.DB) {
	t.Helper()
	got := mustOne(t, ctx, dst, `SELECT code || '|' || name || '|' || status || '|' || huge || '|' || balance || '|' || TO_CHAR(created, 'YYYY-MM-DD HH24:MI:SS.FF9')
			|| '|' || TO_CHAR(seen, 'YYYY-MM-DD HH24:MI:SS.FF TZH:TZM') || '|' || TO_CHAR(tenure) || '|' || TO_CHAR(shift) || '|' || DBMS_LOB.GETLENGTH(bio)
			|| '|' || TO_CHAR(notes) || '|' || RAWTOHEX(DBMS_LOB.SUBSTR(avatar, 10, 1)) || '|' || LENGTH(RAWTOHEX(token)) || '|' || code_upper
		FROM dedie_it_customers WHERE id = 7`)
	want := "c7|user 7 ☃|A|123456789012345678901234567883|8.75|2024-02-29 13:14:15.123456789|2024-02-29 13:14:15.500000 +05:30|+02-06|-01 02:03:04.500000|3034|né ☃ 7|00FF10|32|C7"
	if got != want {
		t.Errorf("row 7:\n got %s\nwant %s", got, want)
	}
	got = mustOne(t, ctx, dst, `SELECT TO_CHAR(wide) || '|' || TO_CHAR(ratio) || '|' || TO_CHAR(single) || '|' || huge || '|' || NVL2(bio, 'empty', 'null') || '|' || NVL2(notes, 'empty', 'null')
			|| '|' || NVL2(avatar, 'empty', 'null') || '|' || TO_CHAR(born, 'YYYY-MM-DD') || '|' || TO_CHAR(seen, 'TZH:TZM') || '|' || TO_CHAR(tenure)
		FROM dedie_it_customers WHERE id = 11`)
	if want := "1.234567890123456789012345678901235E+125|Inf|Nan|-99999999999999999999999999999|empty|null|empty|0001-01-01|-11:00|-99-11"; got != want {
		t.Errorf("row 11:\n got %s\nwant %s", got, want)
	}
	if got := mustOne(t, ctx, dst, `SELECT "Note" FROM "Dedie_It_Mixed" WHERE "Id" = 1`); got != "kept as written" {
		t.Errorf("mixed-case table: %q", got)
	}
	if n, _ := one(ctx, dst, `SELECT COUNT(*) FROM user_tables WHERE table_name = 'DEDIE_IT_MODERN'`); n == "1" {
		if got := mustOne(t, ctx, dst, `SELECT JSON_VALUE(doc, '$.a[1]') || '|' || TO_CHAR(ok) FROM dedie_it_modern WHERE id = 1`); got != "2|TRUE" {
			t.Errorf("JSON and BOOLEAN: %q", got)
		}
	}
}

// checkSchema verifies what was built around the data.
func checkSchema(t *testing.T, ctx context.Context, src, dst *sql.DB) {
	t.Helper()
	for what, q := range map[string]string{
		"foreign key with its delete rule": `SELECT COUNT(*) FROM user_constraints WHERE constraint_name = 'DEDIE_IT_INV_CUST' AND constraint_type = 'R' AND delete_rule = 'CASCADE' AND status = 'ENABLED' AND validated = 'VALIDATED'`,
		"primary key":                      `SELECT COUNT(*) FROM user_constraints WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND constraint_type = 'P' AND status = 'ENABLED'`,
		"unique constraint on its index":   `SELECT COUNT(*) FROM user_constraints WHERE constraint_name = 'DEDIE_IT_CUST_CODE' AND constraint_type = 'U' AND index_name = 'DEDIE_IT_CUST_CODE'`,
		"unique index":                     `SELECT COUNT(*) FROM user_indexes WHERE index_name = 'DEDIE_IT_CUST_CODE' AND uniqueness = 'UNIQUE'`,
		"plain index":                      `SELECT COUNT(*) FROM user_indexes WHERE index_name = 'DEDIE_IT_CUST_NUMS' AND index_type = 'NORMAL'`,
		"function-based index":             `SELECT COUNT(*) FROM user_indexes WHERE index_name = 'DEDIE_IT_CUST_FN' AND index_type = 'FUNCTION-BASED NORMAL' AND status = 'VALID'`,
		"check constraint":                 `SELECT COUNT(*) FROM user_constraints WHERE constraint_name = 'DEDIE_IT_CUST_BAL' AND constraint_type = 'C' AND status = 'ENABLED'`,
		"not null":                         `SELECT COUNT(*) FROM user_tab_columns WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND column_name IN ('CODE', 'STATUS', 'ID') AND nullable = 'N' HAVING COUNT(*) = 3`,
		"sequence":                         `SELECT COUNT(*) FROM user_sequences WHERE sequence_name = 'DEDIE_IT_SEQ' AND increment_by = 5`,
		"identity is ALWAYS again":         `SELECT COUNT(*) FROM user_tab_identity_cols WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND column_name = 'ID' AND generation_type = 'ALWAYS'`,
		"virtual column":                   `SELECT COUNT(*) FROM user_tab_cols WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND column_name = 'CODE_UPPER' AND virtual_column = 'YES'`,
		"partitions":                       `SELECT COUNT(*) FROM user_tab_partitions WHERE table_name = 'DEDIE_IT_PARTS' HAVING COUNT(*) = 2`,
		"table comment":                    `SELECT COUNT(*) FROM user_tab_comments WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND comments = 'who we sell to; it''s the main table'`,
		"column comment":                   `SELECT COUNT(*) FROM user_col_comments WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND column_name = 'CODE' AND comments = 'short name'`,
		"synonym":                          `SELECT COUNT(*) FROM user_synonyms WHERE synonym_name = 'DEDIE_IT_SYN' AND table_owner = USER AND table_name = 'DEDIE_IT_INVOICES'`,
		"trigger, enabled":                 `SELECT COUNT(*) FROM user_triggers WHERE trigger_name = 'DEDIE_IT_AUDIT' AND table_name = 'DEDIE_IT_INVOICES' AND status = 'ENABLED'`,
		"stored code, valid":               `SELECT COUNT(*) FROM user_objects WHERE object_name IN ('DEDIE_IT_TWICE', 'DEDIE_IT_PKG') AND status = 'VALID' HAVING COUNT(*) = 3`,
	} {
		if n, err := one(ctx, dst, q); err != nil || (n != "1" && n != "2" && n != "3") {
			t.Errorf("%s: found %q (%v)", what, n, err)
		}
	}
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_objects WHERE object_name LIKE 'DEDIE_IT%' AND status <> 'VALID'`); n != "0" {
		t.Errorf("%s object(s) on the target are not valid", n)
	}
	// The default calls the sequence of the schema the table is now in.
	var def string
	if err := dst.QueryRowContext(ctx, `SELECT data_default FROM user_tab_columns WHERE table_name = 'DEDIE_IT_CUSTOMERS' AND column_name = 'TICKET'`).Scan(&def); err != nil ||
		!strings.Contains(def, `"`+mustOne(t, ctx, dst, `SELECT USER FROM dual`)+`"."DEDIE_IT_SEQ"`) {
		t.Errorf("default of TICKET: %q (%v)", def, err)
	}
	// Views, synonyms and stored code answer from the target's own rows,
	// and rows lie in the partition they belong to.
	for _, q := range []string{
		`SELECT COUNT(*) || '/' || SUM(total) FROM dedie_it_totals`,
		`SELECT COUNT(*) FROM dedie_it_syn`,
		`SELECT COUNT(*) FROM dedie_it_parts PARTITION (p_low)`,
		`SELECT dedie_it_pkg.invoices FROM dual`,
		`SELECT dedie_it_twice(21) FROM dual`,
	} {
		a, errA := one(ctx, src, q)
		b, errB := one(ctx, dst, q)
		if errA != nil || errB != nil || a != b || a == "" {
			t.Errorf("%s: source %q (%v), target %q (%v)", q, a, errA, b, errB)
		}
	}

	// New rows must get ids and tickets beyond the source's, and the
	// trigger, which was not there while rows arrived, fires for them.
	before := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM dedie_it_events WHERE kind = 'audit'`)
	mustExec(t, ctx, dst, `INSERT INTO dedie_it_customers (code) VALUES ('new on target')`)
	var id, ticket, srcLast float64
	if err := dst.QueryRowContext(ctx, `SELECT id, ticket FROM dedie_it_customers WHERE code = 'new on target'`).Scan(&id, &ticket); err != nil {
		t.Fatal(err)
	}
	if err := src.QueryRowContext(ctx, `SELECT last_number FROM user_sequences WHERE sequence_name = 'DEDIE_IT_SEQ'`).Scan(&srcLast); err != nil {
		t.Fatal(err)
	}
	srcMax, _ := strconv.ParseFloat(mustOne(t, ctx, src, `SELECT MAX(id) FROM dedie_it_customers`), 64)
	if id <= srcMax || ticket < srcLast {
		t.Errorf("identity and sequence were not moved past the source: id %v (source's highest %v), ticket %v (source's sequence at %v)", id, srcMax, ticket, srcLast)
	}
	if _, err := dst.ExecContext(ctx, `INSERT INTO dedie_it_customers (id, code) VALUES (1, 'forced id')`); err == nil {
		t.Error("the identity column accepts a supplied value; it should be GENERATED ALWAYS again")
	}
	mustExec(t, ctx, dst, fmt.Sprintf(`INSERT INTO dedie_it_invoices VALUES (9999999, %v, 1, 'new')`, id))
	if after := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM dedie_it_events WHERE kind = 'audit'`); after == before {
		t.Error("the trigger does not fire on the target")
	}
	mustExec(t, ctx, dst, `DELETE FROM dedie_it_customers WHERE code = 'new on target'`, `DELETE FROM dedie_it_events WHERE payload = 'invoice 9999999'`)
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM dedie_it_invoices WHERE id = 9999999`); n != "0" {
		t.Error("the foreign key does not cascade on the target")
	}
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_tables WHERE table_name = '`+checkpointTable+`'`); n != "0" {
		t.Error("the checkpoint table was left on the target")
	}
}

func logGroups(t *testing.T, ctx context.Context, src *sql.DB) int {
	n, err := strconv.Atoi(mustOne(t, ctx, src, `SELECT COUNT(*) FROM user_log_groups WHERE table_name LIKE 'DEDIE_IT%' OR table_name = 'Dedie_It_Mixed'`))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func find(r *migrate.Report, name string) migrate.Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return migrate.Check{}
}

func TestMigrationOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := setup(t, ctx, fullFixture(t, ctx))

	cfg := migrate.Config{Mode: migrate.ModeOffline}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, c := range report.Checks {
		t.Logf("%-28s %-4s %s", c.Name, c.Status, c.Detail)
		noted = noted || (c.Name == "Offline copy" && c.Status == "warn")
		if c.Name == "Supplemental logging" || c.Name == "LogMiner access" || c.Name == "Primary key logging" {
			t.Errorf("offline preflight checks %q, which only matters when following changes", c.Name)
		}
	}
	if !report.OK || !noted || len(report.Existing) != 0 {
		t.Fatalf("preflight: ok %v, offline note %v, existing %v", report.OK, noted, report.Existing)
	}
	// The preflight says where the schema goes, and that this is a move.
	if c := find(report, "Schema mapping"); c.Status != "ok" || !strings.Contains(c.Detail, "the target account's own schema") {
		t.Errorf("schema mapping: %+v", c)
	}
	if srcCfg.Host == dstCfg.Host {
		if c := find(report, "Versions"); c.Status != "warn" || !strings.Contains(c.Detail, "a move, not an upgrade") {
			t.Errorf("versions: %+v", c)
		}
	}

	// Writes after the copy starts must neither be carried over nor fail
	// the validation, which reads the source as of the snapshot.
	rep := newReporter(t)
	go func() {
		for i := 0; i < 3000; i++ {
			if phase, _ := rep.snapshot(); phase == migrate.PhaseIndexing || phase == migrate.PhaseCutover {
				src.ExecContext(ctx, `INSERT INTO dedie_it_events VALUES ('late', 'written during the copy', SYSDATE)`)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	out, err := Run(ctx, "job-offline", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("run: %v, %v", out, err)
	}
	if phase, _ := rep.snapshot(); phase != migrate.PhaseDone || rep.Desired() != "run" {
		t.Errorf("phase %s, desired %s", phase, rep.Desired())
	}
	if n := logGroups(t, ctx, src); n != 0 {
		t.Errorf("an offline copy switched on supplemental logging for %d table(s)", n)
	}
	for name, f := range rep.tables {
		if v, _ := f["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
			t.Errorf("%s: validation %q", name, v)
		}
	}
	if rep.said("could not create") || rep.said("do not compile") {
		t.Error("some objects were not created or do not compile; see the events above")
	}
	mustExec(t, ctx, src, `DELETE FROM dedie_it_events WHERE kind = 'late'`)

	compare(t, ctx, src, dst, fixtureTables(ctx, src)...)
	checkValues(t, ctx, dst)
	checkSchema(t, ctx, src, dst)
}

// redoLost is what the online test stops an attempt with when the job
// ended because the source moved on to another redo log.
type redoLost struct{ err error }

// TestMigration follows a source in use. A source that does not archive
// its redo logs, reached through a pluggable database, lets LogMiner
// read only the log being written: when the server switches logs while
// the job still has changes to read, they are out of reach and the job
// says so and ends. That is the documented behaviour, not a fault in
// the migration, and on a small test server shared with other tests it
// happens every few minutes, so the test starts over when it does. The
// fixture keeps its large values to a few rows for the same reason: the
// less redo the tests write, the less often the logs switch.
func TestMigration(t *testing.T) {
	const attempts = 4
	for attempt := 1; ; attempt++ {
		lost := func() (lost *redoLost) {
			defer func() {
				if p := recover(); p != nil {
					l, ok := p.(redoLost)
					if !ok {
						panic(p)
					}
					lost = &l
				}
			}()
			onlineMigration(t)
			return nil
		}()
		if lost == nil {
			return
		}
		if attempt == attempts {
			t.Fatalf("the source switched redo logs during each of %d attempts: %v", attempts, lost.err)
		}
		t.Logf("attempt %d: the source switched redo logs under the job (%v); starting over", attempt, lost.err)
	}
}

func onlineMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := setup(t, ctx, fullFixture(t, ctx))

	cfg := migrate.Config{}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		t.Logf("%-28s %-4s %s", c.Name, c.Status, c.Detail)
	}
	if !report.OK {
		t.Fatal("preflight failed")
	}

	rep := newReporter(t)
	type result struct {
		out migrate.Outcome
		err error
	}
	done := make(chan result, 1)
	// ended is called when the job returned something other than expected.
	ended := func(what string, r result) {
		t.Helper()
		if errors.As(r.err, &lostRedo{}) {
			panic(redoLost{r.err})
		}
		t.Fatalf("%s: %v, %v", what, r.out, r.err)
	}
	start := func(st State) {
		go func() {
			out, err := Run(ctx, "job-online", cfg, st, srcCfg, dstCfg, rep)
			done <- result{out, err}
		}()
	}
	waitPhase := func(want string) {
		t.Helper()
		for i := 0; i < 6000; i++ {
			select {
			case r := <-done:
				ended("job ended while waiting for "+want, r)
			default:
			}
			if phase, _ := rep.snapshot(); phase == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for phase %s", want)
	}
	// converged waits until a query on the target returns what is expected.
	converged := func(what, query, want string) {
		t.Helper()
		var got string
		for i := 0; i < 900; i++ {
			select {
			case r := <-done:
				ended("job ended while waiting for "+what, r)
			default:
			}
			var err error
			if got, err = one(ctx, dst, query); err == nil && got == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: target has %q, want %q", what, got, want)
	}

	// A transaction left open across the snapshot: its changes are in
	// neither the copy nor, unless mining starts early enough, the stream.
	// Key logging for its table is switched on by hand first, because the
	// job could not do so while the transaction holds the table; the job
	// must then leave that setting as it found it.
	mustExec(t, ctx, src, `ALTER TABLE dedie_it_customers ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS`)
	early, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Should the attempt be given up, the transaction must not go on
	// holding the table.
	defer early.Rollback()
	mustExec(t, ctx, early, `UPDATE dedie_it_customers SET balance = 4242 WHERE code = 'c10'`)

	start(State{})
	waitPhase(migrate.PhaseStreaming)
	if n := logGroups(t, ctx, src); n < 4 {
		t.Errorf("primary key logging is on for %d table(s), want at least 4", n)
	}
	// While rows are still arriving the target has neither the foreign
	// key nor the trigger.
	if n := mustOne(t, ctx, dst, `SELECT (SELECT COUNT(*) FROM user_triggers WHERE trigger_name = 'DEDIE_IT_AUDIT') + (SELECT COUNT(*) FROM user_constraints WHERE constraint_type = 'R') FROM dual`); n != "0" {
		t.Error("the trigger or the foreign key exists on the target before cutover")
	}
	if err := early.Commit(); err != nil {
		t.Fatal(err)
	}
	converged("transaction open across the snapshot", `SELECT TO_CHAR(balance) FROM dedie_it_customers WHERE code = 'c10'`, "4242")

	// Ordinary changes of every kind, including large objects rewritten
	// in place, emptied and set to NULL, and a change of primary key.
	mustExec(t, ctx, src,
		`INSERT INTO dedie_it_customers (code, name, balance, bio, avatar, shift, seen) VALUES ('fresh', N'new ☃ row', 12.5, TO_CLOB(RPAD('q', 4000, 'q')) || RPAD('q', 4000, 'q') || RPAD('q', 1000, 'q'), HEXTORAW('CAFE'), INTERVAL '3 00:00:01' DAY TO SECOND, TIMESTAMP '2025-06-01 08:00:00.25 -03:00')`,
		`UPDATE dedie_it_customers SET bio = TO_CLOB(RPAD('w', 4000, 'w')) || RPAD('w', 4000, 'w') || RPAD('w', 4000, 'w') || bio, born = DATE '2001-02-03', small = 42 WHERE code = 'c7'`,
		`UPDATE dedie_it_customers SET balance = balance + 1 WHERE id BETWEEN 100 AND 1100`,
		`UPDATE dedie_it_customers SET bio = EMPTY_CLOB(), avatar = NULL WHERE code = 'c20'`,
		`UPDATE dedie_it_customers SET bio = NULL, avatar = EMPTY_BLOB(), wide = 1.5E+120 WHERE code = 'c21'`,
		`DELETE FROM dedie_it_customers WHERE id BETWEEN 2000 AND 2100`,
		`UPDATE dedie_it_invoices SET id = id + 1000000 WHERE id BETWEEN 1 AND 50`,
		`INSERT INTO dedie_it_invoices VALUES (5000001, 5, 10, 'added')`,
		`UPDATE "Dedie_It_Mixed" SET "Note" = 'changed' WHERE "Id" = 1`,
		`INSERT INTO "Dedie_It_Mixed" VALUES (2, 'it''s, "quoted" and = and')`,
		`UPDATE dedie_it_parts SET id = id + 1000 WHERE id <= 10`,
		`INSERT INTO dedie_it_events VALUES ('after', 'not followed: no primary key', SYSDATE)`,
	)
	// A rolled-back transaction leaves nothing behind.
	tx, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tx, `UPDATE dedie_it_customers SET code = 'rolled back' WHERE code = 'c5'`, `DELETE FROM dedie_it_invoices WHERE id > 2900`)
	tx.Rollback()
	// So does the part of a transaction undone back to a savepoint, while
	// what came before the savepoint stays.
	mustExec(t, ctx, src, `BEGIN
			UPDATE dedie_it_customers SET medium = 77 WHERE code = 'c6';
			SAVEPOINT sp;
			DELETE FROM dedie_it_customers WHERE code = 'c8';
			UPDATE dedie_it_customers SET medium = 78 WHERE code = 'c9';
			INSERT INTO "Dedie_It_Mixed" VALUES (3, 'undone');
			ROLLBACK TO sp;
			COMMIT;
		END;`)
	// Unique values passed between rows, inside one transaction and across two.
	tx, err = src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tx,
		`UPDATE dedie_it_customers SET code = 'swap' WHERE code = 'c1'`,
		`UPDATE dedie_it_customers SET code = 'c1' WHERE code = 'c2'`,
		`UPDATE dedie_it_customers SET code = 'c2' WHERE code = 'swap'`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, src,
		`UPDATE dedie_it_customers SET code = 'moving' WHERE code = 'c3'`,
		`UPDATE dedie_it_customers SET code = 'c3' WHERE code = 'c4'`)

	converged("insert", `SELECT TO_CHAR(name) || '|' || balance || '|' || DBMS_LOB.GETLENGTH(bio) || '|' || RAWTOHEX(DBMS_LOB.SUBSTR(avatar, 10, 1)) || '|' || TO_CHAR(shift) || '|' || TO_CHAR(seen, 'HH24:MI:SS.FF2 TZH:TZM') FROM dedie_it_customers WHERE code = 'fresh'`,
		"new ☃ row|12.5|9000|CAFE|+03 00:00:01.000000|08:00:00.25 -03:00")
	converged("large object update", `SELECT DBMS_LOB.GETLENGTH(bio) || '|' || TO_CHAR(born, 'YYYY-MM-DD HH24:MI:SS') || '|' || small FROM dedie_it_customers WHERE id = 7`, "15034|2001-02-03 00:00:00|42")
	converged("large object emptied", `SELECT NVL2(bio, 'empty', 'null') || '|' || NVL2(avatar, 'empty', 'null') || '|' || DBMS_LOB.GETLENGTH(bio) FROM dedie_it_customers WHERE code = 'c20'`, "empty|null|0")
	converged("large object set to NULL", `SELECT NVL2(bio, 'empty', 'null') || '|' || NVL2(avatar, 'empty', 'null') || '|' || TO_CHAR(wide) FROM dedie_it_customers WHERE code = 'c21'`,
		"null|empty|1.500000000000000000000000000000000E+120")
	converged("primary key update", `SELECT COUNT(*) FROM dedie_it_invoices WHERE id > 1000000`, "51")
	converged("primary key update across partitions", `SELECT COUNT(*) FROM dedie_it_parts PARTITION (p_high) WHERE id > 1000`, "10")
	converged("unique swap", `SELECT LISTAGG(code, ',') WITHIN GROUP (ORDER BY id) FROM dedie_it_customers WHERE id IN (1, 2, 3, 4)`, "c2,c1,moving,c3")
	converged("mixed-case table", `SELECT LISTAGG("Note", ';') WITHIN GROUP (ORDER BY "Id") FROM "Dedie_It_Mixed"`, `changed;it's, "quoted" and = and`)
	converged("rollback to a savepoint", `SELECT LISTAGG(code || '=' || medium, ',') WITHIN GROUP (ORDER BY id) FROM dedie_it_customers WHERE id IN (6, 8, 9)`, "c6=77,c8=8000,c9=9000")
	converged("rollback", `SELECT COUNT(*) FROM dedie_it_customers WHERE code = 'rolled back'`, "0")

	// Pause, change the source while nothing is listening, and resume.
	rep.SetDesired("pause")
	if r := <-done; r.err != nil || r.out != migrate.Paused {
		ended("pause", r)
	}
	mustExec(t, ctx, src,
		`UPDATE dedie_it_customers SET status = 'P' WHERE id <= 500`,
		`DELETE FROM dedie_it_customers WHERE id BETWEEN 2500 AND 2520`,
		`TRUNCATE TABLE dedie_it_events`,
	)
	_, st := rep.snapshot()
	rep.SetDesired("run")
	start(st)
	converged("changes made while paused", `SELECT COUNT(*) FROM dedie_it_customers WHERE status = 'P'`, "500")
	converged("truncate", `SELECT COUNT(*) FROM dedie_it_events`, "0")

	rep.SetDesired("cutover")
	if r := <-done; r.err != nil || r.out != migrate.Completed {
		ended("cutover", r)
	}
	_, st = rep.snapshot()
	t.Logf("applied %d transaction(s), %d row(s), %d anomalies", st.TxApplied, st.RowsApplied, st.Anomalies)
	for name, f := range rep.tables {
		if v, _ := f["validation"].(string); !strings.Contains(v, "match") || v == "mismatch" {
			t.Errorf("%s: validation %q", name, v)
		}
	}
	if rep.said("could not create") || rep.said("do not compile") {
		t.Error("some objects were not created or do not compile; see the events above")
	}
	if n := logGroups(t, ctx, src); n != 1 {
		t.Errorf("primary key logging is on for %d table(s) afterwards; only the one switched on before the job should remain", n)
	}
	// Every row of every table, value for value. The cascade ran on the
	// source; the target must not have kept the invoices of deleted
	// customers, nor lost any other.
	compare(t, ctx, src, dst, fixtureTables(ctx, src)...)
	checkSchema(t, ctx, src, dst)
}

// What the target holds, before the migration, under names the source
// uses too. Each is recognisably the target's own.
var clashing = []string{
	`CREATE TABLE dedie_it_keep (id NUMBER PRIMARY KEY, mine VARCHAR2(30))`,
	`INSERT INTO dedie_it_keep VALUES (1, 'the target''s own row')`,
	`CREATE VIEW dedie_it_v AS SELECT 'target' origin FROM dual`,
	`CREATE SEQUENCE dedie_it_s START WITH 7 NOCACHE`,
	`CREATE FUNCTION dedie_it_f RETURN VARCHAR2 IS BEGIN RETURN 'target'; END;`,
	`CREATE TRIGGER dedie_it_t BEFORE INSERT ON dedie_it_keep FOR EACH ROW BEGIN :NEW.mine := NVL(:NEW.mine, 'target trigger'); END;`,
}

// The source's objects of the same names, and a table that only it has.
var clashingSource = []string{
	`CREATE SEQUENCE dedie_it_s START WITH 5000 NOCACHE`,
	`CREATE TABLE dedie_it_keep (id NUMBER PRIMARY KEY, theirs VARCHAR2(30), n NUMBER DEFAULT dedie_it_s.NEXTVAL)`,
	`INSERT INTO dedie_it_keep (id, theirs) SELECT LEVEL, 'source row ' || LEVEL FROM dual CONNECT BY LEVEL <= 20`,
	`CREATE TABLE dedie_it_other (id NUMBER PRIMARY KEY, keep_id NUMBER, note VARCHAR2(30),
		CONSTRAINT dedie_it_other_keep FOREIGN KEY (keep_id) REFERENCES dedie_it_keep (id))`,
	`CREATE INDEX dedie_it_other_note ON dedie_it_other (note)`,
	`INSERT INTO dedie_it_other SELECT LEVEL, 1, 'note ' || LEVEL FROM dual CONNECT BY LEVEL <= 50`,
	`CREATE VIEW dedie_it_v AS SELECT 'source' origin FROM dual`,
	`CREATE FUNCTION dedie_it_f RETURN VARCHAR2 IS BEGIN RETURN 'source'; END;`,
	`CREATE TRIGGER dedie_it_t BEFORE INSERT ON dedie_it_other FOR EACH ROW BEGIN :NEW.note := NVL(:NEW.note, 'source trigger'); END;`,
}

// fingerprint describes the clashing objects on a database closely
// enough to tell whose they are and whether anything touched them.
func fingerprint(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT LISTAGG(column_name, ',') WITHIN GROUP (ORDER BY column_id) FROM user_tab_columns WHERE table_name = 'DEDIE_IT_KEEP'`,
		`SELECT COUNT(*) FROM dedie_it_keep`,
		`SELECT TO_CHAR(created, 'YYYYMMDDHH24MISS') || '/' || TO_CHAR(last_ddl_time, 'YYYYMMDDHH24MISS') FROM user_objects WHERE object_name = 'DEDIE_IT_KEEP' AND object_type = 'TABLE'`,
		`SELECT origin FROM dedie_it_v`,
		`SELECT TO_CHAR(last_ddl_time, 'YYYYMMDDHH24MISS') FROM user_objects WHERE object_name = 'DEDIE_IT_V'`,
		`SELECT dedie_it_f FROM dual`,
		`SELECT LISTAGG(text) WITHIN GROUP (ORDER BY line) FROM user_source WHERE name = 'DEDIE_IT_F'`,
		`SELECT TO_CHAR(last_ddl_time, 'YYYYMMDDHH24MISS') FROM user_objects WHERE object_name = 'DEDIE_IT_F'`,
		`SELECT last_number FROM user_sequences WHERE sequence_name = 'DEDIE_IT_S'`,
		`SELECT table_name || '/' || status FROM user_triggers WHERE trigger_name = 'DEDIE_IT_T'`,
	} {
		v, err := one(ctx, db, q)
		if err != nil {
			v = "error: " + err.Error()
		}
		parts = append(parts, strings.TrimSpace(v))
	}
	return strings.Join(parts, " | ")
}

// TestMigrationExistingObjects takes, on the target, the names the
// migration wants for a table, a view, a sequence, a function and a
// trigger, and runs it under each of the three policies.
func TestMigrationExistingObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := setup(t, ctx, clashingSource)
	mustExec(t, ctx, dst, clashing...)
	theirs := fingerprint(t, ctx, dst)
	t.Logf("the target's own: %s", theirs)
	user := mustOne(t, ctx, dst, `SELECT USER FROM dual`)
	wantExisting := map[migrate.ExistingObject]bool{
		{Kind: "table", Name: user + ".DEDIE_IT_KEEP"}: true, {Kind: "view", Name: user + ".DEDIE_IT_V"}: true, {Kind: "sequence", Name: user + ".DEDIE_IT_S"}: true,
		{Kind: "function", Name: user + ".DEDIE_IT_F"}: true, {Kind: "trigger", Name: user + ".DEDIE_IT_T"}: true,
	}
	preflight := func(policy, status string) *migrate.Report {
		t.Helper()
		report, err := Preflight(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline, OnExisting: policy})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Existing) != len(wantExisting) {
			t.Errorf("%s: existing %v", policy, report.Existing)
		}
		for _, e := range report.Existing {
			if !wantExisting[e] {
				t.Errorf("%s: unexpected existing object %+v", policy, e)
			}
		}
		tables, others := find(report, "Target is empty"), find(report, "Existing objects")
		t.Logf("%s: %s: %s", policy, tables.Status, tables.Detail)
		t.Logf("%s: %s: %s", policy, others.Status, others.Detail)
		if tables.Status != status || others.Status != status || report.OK != (status != "fail") || !strings.Contains(tables.Detail, user+".DEDIE_IT_KEEP") ||
			!strings.Contains(others.Detail, "views: "+user+".DEDIE_IT_V") || !strings.Contains(others.Detail, "triggers: "+user+".DEDIE_IT_T") {
			t.Errorf("%s: checks %+v %+v, ok %v", policy, tables, others, report.OK)
		}
		return report
	}

	// By default the job refuses to start and touches nothing.
	report := preflight("", "fail")
	if d := find(report, "Existing objects").Detail; !strings.Contains(d, "never replaces or merges") {
		t.Errorf("the refusal does not say why: %s", d)
	}
	rep := newReporter(t)
	out, err := Run(ctx, "job-existing-fail", migrate.Config{Mode: migrate.ModeOffline}, State{}, srcCfg, dstCfg, rep)
	if err == nil || out == migrate.Completed || !strings.Contains(err.Error(), "Existing objects") || !strings.Contains(err.Error(), "Target is empty") {
		t.Fatalf("a job over existing objects ran: %v, %v", out, err)
	}
	if got := fingerprint(t, ctx, dst); got != theirs {
		t.Errorf("the refused job changed the target:\n was %s\n now %s", theirs, got)
	}
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_tables WHERE table_name IN ('DEDIE_IT_OTHER', '`+checkpointTable+`')`); n != "0" {
		t.Error("the refused job created something on the target")
	}

	// Keeping: every existing object stays exactly as it was, and the
	// rest is migrated around it.
	report = preflight(migrate.ExistingSkip, "warn")
	if d := find(report, "Target is empty").Detail; !strings.Contains(d, "kept exactly as they are") {
		t.Errorf("keeping is not explained: %s", d)
	}
	rep = newReporter(t)
	cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}
	out, err = Run(ctx, "job-existing-skip", cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("skip: %v, %v", out, err)
	}
	if got := fingerprint(t, ctx, dst); got != theirs {
		t.Errorf("keeping changed what was kept:\n was %s\n now %s", theirs, got)
	}
	_, st := rep.snapshot()
	if !st.ExistingDecided || strings.Join(st.SkippedTables, ",") != user+".DEDIE_IT_KEEP" || len(st.SkippedObjects) != 5 || !rep.said("kept 1 existing table(s) and 4 other object(s)") {
		t.Errorf("the decision was not remembered: %+v", st)
	}
	if _, kept := rep.tables["DEDIE_IT_KEEP"]; kept || len(rep.tables) != 1 {
		t.Errorf("the kept table took part in the migration: %v", rep.tables)
	}
	compare(t, ctx, src, dst, "dedie_it_other")
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_indexes WHERE index_name = 'DEDIE_IT_OTHER_NOTE'`); n != "1" {
		t.Error("the index of the migrated table is missing")
	}
	// A second run of the same job, as after a restart, must go by what
	// it remembers: by now its own table exists on the target as well.
	out, err = Run(ctx, "job-existing-skip", cfg, State{ExistingDecided: true, SkippedTables: st.SkippedTables, SkippedSources: st.SkippedSources, SkippedObjects: st.SkippedObjects},
		srcCfg, dstCfg, newReporter(t))
	if err != nil || out != migrate.Completed {
		t.Fatalf("skip, run again from its remembered decision: %v, %v", out, err)
	}
	if got := fingerprint(t, ctx, dst); got != theirs {
		t.Errorf("the second run changed what was kept:\n was %s\n now %s", theirs, got)
	}
	compare(t, ctx, src, dst, "dedie_it_other")

	// Replacing: what was there is dropped, and the source's takes its
	// place. The table migrated a moment ago is now in the way too; its
	// index and the trigger go with the tables they are on.
	wantExisting[migrate.ExistingObject{Kind: "table", Name: user + ".DEDIE_IT_OTHER"}] = true
	wantExisting[migrate.ExistingObject{Kind: "index", Name: user + ".DEDIE_IT_OTHER_NOTE"}] = true
	report = preflight(migrate.ExistingReplace, "warn")
	if d := find(report, "Target is empty").Detail; !strings.Contains(d, "DROPPED") {
		t.Errorf("replacing does not say what it destroys: %s", d)
	}
	rep = newReporter(t)
	out, err = Run(ctx, "job-existing-replace", migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace}, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("replace: %v, %v", out, err)
	}
	if !rep.said("dropped 2 existing table(s) and 3 other object(s) on the target, as chosen") {
		t.Error("the drops were not reported")
	}
	if rep.said("could not create") || rep.said("do not compile") {
		t.Error("some objects were not created or do not compile; see the events above")
	}
	compare(t, ctx, src, dst, "dedie_it_keep", "dedie_it_other")
	for what, q := range map[string]string{
		"view":        `SELECT origin FROM dedie_it_v`,
		"function":    `SELECT dedie_it_f FROM dual`,
		"trigger":     `SELECT table_name || '/' || status FROM user_triggers WHERE trigger_name = 'DEDIE_IT_T'`,
		"sequence":    `SELECT last_number FROM user_sequences WHERE sequence_name = 'DEDIE_IT_S'`,
		"foreign key": `SELECT status FROM user_constraints WHERE constraint_name = 'DEDIE_IT_OTHER_KEEP'`,
	} {
		a, errA := one(ctx, src, q)
		b, errB := one(ctx, dst, q)
		if errA != nil || errB != nil || a != b {
			t.Errorf("%s after replacing: source %q (%v), target %q (%v)", what, a, errA, b, errB)
		}
	}
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_tables WHERE table_name = '`+checkpointTable+`'`); n != "0" {
		t.Error("the checkpoint table was left on the target")
	}
}

// named lists the tests' objects in a schema, by type and name.
func named(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT object_type || ' ' || object_name FROM user_objects
		WHERE UPPER(object_name) LIKE 'DEDIE\_IT\_%' ESCAPE '\' AND object_type NOT LIKE '%PARTITION' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// TestMigrationPreview reads the statements of a migration before it is
// started. A preview must be the real script, so the test runs it, as it
// is, against the target, and finds every object of the source there; and
// it must be only a preview, so both schemas are compared with what they
// were before.
func TestMigrationPreview(t *testing.T) {
	var _ migrate.Previewer = pipeline{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := setup(t, ctx, fixture)
	user := mustOne(t, ctx, dst, `SELECT USER FROM dual`)
	source := named(t, ctx, src)

	// A job that follows changes would switch key logging on at the
	// source; its preview must not.
	list, err := Preview(ctx, srcCfg, dstCfg, migrate.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := named(t, ctx, src); got != source || logGroups(t, ctx, src) != 0 {
		t.Errorf("the preview changed the source:\n%s", got)
	}
	if got := named(t, ctx, dst); got != "" {
		t.Errorf("the preview created on the target:\n%s", got)
	}

	order := map[string]int{migrate.PhaseSchema: 1, migrate.PhaseIndexing: 2, migrate.PhaseCutover: 3}
	last := 0
	at := map[string]string{}
	for _, s := range list {
		if order[s.Phase] < last || order[s.Phase] == 0 || s.Kind == "" || s.Name == "" || strings.TrimSpace(s.SQL) == "" {
			t.Errorf("%s | %s | %s: out of order or incomplete", s.Phase, s.Kind, s.Name)
		}
		last = order[s.Phase]
		if _, ok := at[s.Kind+" "+s.Name]; !ok {
			at[s.Kind+" "+s.Name] = s.Phase + s.End
		}
		// Everything is written for the receiving schema.
		if strings.Contains(s.SQL, `"`+srcCfg.Username+`"`) || strings.Contains(s.SQL, `"`+strings.ToUpper(srcCfg.Username)+`"`) {
			t.Errorf("%s %s still names the source schema:\n%s", s.Kind, s.Name, s.SQL)
		}
	}
	// Tables and sequences before the copy; code, views and indexes after
	// it; foreign keys and triggers at the very end. A program unit is
	// closed with a slash in a script.
	for name, want := range map[string]string{
		"sequence " + user + ".DEDIE_IT_SEQ":                           migrate.PhaseSchema,
		"table " + user + ".DEDIE_IT_CUSTOMERS":                        migrate.PhaseSchema,
		"table " + user + ".Dedie_It_Mixed":                            migrate.PhaseSchema,
		"function " + user + ".DEDIE_IT_TWICE":                         migrate.PhaseIndexing + "/",
		"package body " + user + ".DEDIE_IT_PKG":                       migrate.PhaseIndexing + "/",
		"view " + user + ".DEDIE_IT_TOTALS":                            migrate.PhaseIndexing,
		"synonym " + user + ".DEDIE_IT_SYN":                            migrate.PhaseIndexing,
		"index " + user + ".DEDIE_IT_CUST_NUMS":                        migrate.PhaseIndexing,
		"foreign key " + user + ".DEDIE_IT_INVOICES DEDIE_IT_INV_CUST": migrate.PhaseCutover,
		"trigger " + user + ".DEDIE_IT_AUDIT":                          migrate.PhaseCutover + "/",
	} {
		if got, ok := at[name]; !ok || got != want {
			t.Errorf("%s: previewed at %q, want %q", name, got, want)
		}
	}

	// The script, run as it is written, builds the source's schema.
	for _, s := range list {
		if _, err := dst.ExecContext(ctx, s.SQL); err != nil {
			t.Errorf("%s %s does not run: %v\n%s", s.Kind, s.Name, err, s.SQL)
		}
	}
	if got := named(t, ctx, dst); got != source {
		t.Errorf("the script built:\n%s\nthe source has:\n%s", got, source)
	}
	mustExec(t, ctx, dst, `BEGIN DBMS_UTILITY.COMPILE_SCHEMA(USER, FALSE); END;`)
	if n := mustOne(t, ctx, dst, `SELECT COUNT(*) FROM user_objects WHERE status <> 'VALID' AND object_name LIKE 'DEDIE_IT%'`); n != "0" {
		t.Errorf("%s object(s) built by the script do not compile", n)
	}

	// Everything is on the target now. Keeping it leaves nothing to do.
	if _, err := Preview(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}); err == nil || !strings.Contains(err.Error(), "nothing to migrate") {
		t.Errorf("a preview with every table kept: %v", err)
	}
	// Replacing it drops first: tables before the rest, and nothing that
	// went with a table already. The drops run as written too.
	list, err = Preview(ctx, srcCfg, dstCfg, migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace})
	if err != nil {
		t.Fatal(err)
	}
	drops, tablesDone := 0, false
	for _, s := range list {
		if s.Kind != "drop" {
			break
		}
		drops++
		isTable := strings.HasPrefix(s.SQL, "DROP TABLE ")
		if isTable && tablesDone {
			t.Errorf("%s comes after other objects were dropped", s.SQL)
		}
		tablesDone = tablesDone || !isTable
		if strings.HasPrefix(s.SQL, "DROP INDEX ") || strings.HasPrefix(s.SQL, "DROP TRIGGER ") {
			t.Errorf("%s: its table is dropped before it", s.SQL)
		}
		if _, err := dst.ExecContext(ctx, s.SQL); err != nil {
			t.Errorf("%s does not run: %v", s.SQL, err)
		}
	}
	if drops == 0 || !tablesDone {
		t.Errorf("replacing previews %d drop(s)", drops)
	}
	if got := named(t, ctx, dst); got != "" {
		t.Errorf("the drops left:\n%s", got)
	}
}
