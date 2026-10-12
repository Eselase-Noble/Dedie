package codesql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	go_ora "github.com/sijms/go-ora/v2"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
)

// The integration tests prove a conversion by running it. For each
// route they create a set of realistic routines on the source database,
// run them there with fixed inputs, convert them, create the result on
// the target database, run the same inputs and compare what came back
// and what the tables hold afterwards. They need the same servers as
// the pipelines' own tests:
//
//	DEDIE_TEST_ORACLE_SOURCE=oracle://dedie_test:secret@127.0.0.1:1521/FREEPDB1
//	DEDIE_TEST_MYSQL_SOURCE=mysql://root:secret@127.0.0.1:33161
//	DEDIE_TEST_SOURCE=postgres://postgres@127.0.0.1:54316/postgres
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
//	DEDIE_TEST_MYSQL_TARGET=mysql://root:secret@127.0.0.1:33162
//
// In Oracle everything is created in the account's own schema under the
// prefix DEDIE_CS_ and dropped again. On PostgreSQL and MySQL the tests
// work in a schema or database of their own.

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

// engine is one side of a comparison: a database the tests can run
// statements on in the same terms whichever product it is.
type engine interface {
	// exec runs a statement.
	exec(stmt string) error
	// query runs a query and returns its rows as text.
	query(q string) ([][]string, error)
	// function evaluates an expression that calls a function.
	function(expr string) ([][]string, error)
	// procedure calls a procedure with the given arguments, written as
	// SQL literals, followed by outs output parameters, and returns
	// their values.
	procedure(name string, in []string, outs int) ([]string, error)
	// source returns the definition of a routine as the catalog has it.
	source(kind, name, table string) (string, error)
	// drop removes a routine if it exists.
	drop(kind, name, table string)
	// tables describes the fixture's tables as Options wants them.
	tables(names []string) ([]Table, error)
	close()
}

const testTimeout = 5 * time.Minute

// Oracle.

type oraEngine struct {
	t    *testing.T
	ctx  context.Context
	db   *sql.DB
	conn *sql.Conn
}

// waitForOracle holds a test back while another test binary is working
// in the same Oracle schema: the migration pipelines' tests rebuild it
// and count its objects.
func waitForOracle(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	for i := 0; i < 360; i++ {
		out, _ := exec.Command("pgrep", "-f", `ora(pg|my|ora|src)\.test|codesql\.test`).Output()
		busy := false
		for _, pid := range strings.Fields(string(out)) {
			if pid != self {
				busy = true
			}
		}
		if !busy {
			return
		}
		if i == 0 {
			t.Log("another Oracle test is running; waiting for it to finish")
		}
		time.Sleep(5 * time.Second)
	}
	t.Skip("another Oracle test is still running")
}

func openOracle(t *testing.T, ctx context.Context) *oraEngine {
	cfg := testConfig(t, "DEDIE_TEST_ORACLE_SOURCE")
	waitForOracle(t)
	db, err := oracle.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := &oraEngine{t: t, ctx: ctx, db: db, conn: conn}
	// OUT parameters come back as text; a fixed format keeps dates
	// comparable. The setting is the session's own.
	if err := e.exec(`ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS'`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *oraEngine) close() { e.conn.Close(); e.db.Close() }

func (e *oraEngine) exec(stmt string) error {
	_, err := e.conn.ExecContext(e.ctx, stmt)
	return err
}

func (e *oraEngine) query(q string) ([][]string, error) { return sqlRows(e.ctx, e.conn, q) }

func (e *oraEngine) function(expr string) ([][]string, error) {
	return e.query("SELECT " + expr + " FROM dual")
}

func (e *oraEngine) procedure(name string, in []string, outs int) ([]string, error) {
	args := append([]string{}, in...)
	dests := make([]string, outs)
	var binds []any
	for i := range dests {
		args = append(args, fmt.Sprintf(":o%d", i+1))
		binds = append(binds, go_ora.Out{Dest: &dests[i], Size: 4000})
	}
	// The driver takes ORA-01403 for the end of a result rather than an
	// error, so NO_DATA_FOUND leaving the procedure is given a number
	// it cannot mistake.
	_, err := e.conn.ExecContext(e.ctx, "BEGIN "+name+"("+strings.Join(args, ", ")+
		"); EXCEPTION WHEN NO_DATA_FOUND THEN RAISE_APPLICATION_ERROR(-20999, 'no data found'); END;", binds...)
	if err != nil {
		return nil, err
	}
	for i := range dests {
		if dests[i] == "" {
			dests[i] = null
		}
	}
	return dests, nil
}

// create compiles a routine and fails the test if Oracle found errors
// in it, which it reports apart from the statement itself.
func (e *oraEngine) create(t *testing.T, ddl string, name string) {
	if err := e.exec(ddl); err != nil {
		t.Fatalf("creating %s on Oracle: %v", name, err)
	}
	rows, err := e.query(`SELECT line || ': ' || text FROM user_errors WHERE name = '` + strings.ToUpper(name) + `' ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 0 {
		t.Fatalf("%s does not compile on Oracle: %v", name, rows)
	}
}

func (e *oraEngine) source(kind, name, table string) (string, error) {
	rows, err := e.query(`SELECT text FROM user_source WHERE name = '` + strings.ToUpper(name) + `' AND type = '` + strings.ToUpper(kind) + `' ORDER BY line`)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r[0])
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("USER_SOURCE has nothing for %s", name)
	}
	return b.String(), nil
}

func (e *oraEngine) drop(kind, name, table string) { e.exec("DROP " + kind + " " + name) }

func (e *oraEngine) tables(names []string) ([]Table, error) {
	var out []Table
	for _, name := range names {
		rows, err := e.conn.QueryContext(e.ctx, `SELECT column_name, data_type, char_length, data_precision, data_scale, data_length
			FROM user_tab_columns WHERE table_name = :1 ORDER BY column_id`, strings.ToUpper(name))
		if err != nil {
			return nil, err
		}
		t := Table{Name: strings.ToUpper(name)}
		for rows.Next() {
			var col, typ string
			var chars, prec, scale sql.NullInt64
			var length int64
			if err := rows.Scan(&col, &typ, &chars, &prec, &scale, &length); err != nil {
				return nil, err
			}
			t.Columns = append(t.Columns, Column{Name: col, Type: oracle.TypeName(typ, chars, prec, scale, length)})
		}
		rows.Close()
		if len(t.Columns) == 0 {
			return nil, fmt.Errorf("Oracle has no table %s", name)
		}
		out = append(out, t)
	}
	return out, nil
}

// PostgreSQL.

type pgEngine struct {
	t      *testing.T
	ctx    context.Context
	conn   *pgx.Conn
	schema string
}

// openPostgres connects and makes a fresh schema the session works in.
func openPostgres(t *testing.T, ctx context.Context, env, schema string) *pgEngine {
	cfg := testConfig(t, env)
	pc, err := postgres.ConnConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Results are compared as the text the server writes.
	pc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pc.RuntimeParams["timezone"] = "UTC"
	pc.RuntimeParams["datestyle"] = "ISO"
	conn, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	e := &pgEngine{t: t, ctx: ctx, conn: conn, schema: schema}
	for _, stmt := range []string{"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema, "SET search_path = " + schema} {
		if err := e.exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *pgEngine) close() {
	e.exec("DROP SCHEMA IF EXISTS " + e.schema + " CASCADE")
	e.conn.Close(context.Background())
}

func (e *pgEngine) exec(stmt string) error {
	_, err := e.conn.Exec(e.ctx, stmt)
	return err
}

func (e *pgEngine) query(q string) ([][]string, error) {
	rows, err := e.conn.Query(e.ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var row []string
		for _, v := range rows.RawValues() {
			if v == nil {
				row = append(row, null)
			} else {
				row = append(row, normalize(string(v)))
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (e *pgEngine) function(expr string) ([][]string, error) { return e.query("SELECT " + expr) }

func (e *pgEngine) procedure(name string, in []string, outs int) ([]string, error) {
	args := append([]string{}, in...)
	for i := 0; i < outs; i++ {
		args = append(args, "NULL")
	}
	rows, err := e.query("CALL " + name + "(" + strings.Join(args, ", ") + ")")
	if err != nil {
		return nil, err
	}
	if outs == 0 {
		return []string{}, nil
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("CALL %s returned %d rows", name, len(rows))
	}
	return rows[0], nil
}

func (e *pgEngine) source(kind, name, table string) (string, error) {
	if kind == "trigger" {
		rows, err := e.query(`SELECT pg_get_triggerdef(t.oid) || ';' || chr(10) || pg_get_functiondef(t.tgfoid)
			FROM pg_trigger t WHERE t.tgname = '` + name + `' AND t.tgrelid = '` + e.schema + `.` + table + `'::regclass`)
		if err != nil || len(rows) != 1 {
			return "", fmt.Errorf("reading the trigger %s: %v (%d rows)", name, err, len(rows))
		}
		return rows[0][0], nil
	}
	// The raw text: definitions are code, not values to normalize.
	var def string
	err := e.conn.QueryRow(e.ctx, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = '`+e.schema+`' AND p.proname = '`+name+`'`).Scan(&def)
	return def, err
}

func (e *pgEngine) drop(kind, name, table string) {
	if kind == "trigger" {
		e.exec("DROP TRIGGER IF EXISTS " + name + " ON " + table)
		return
	}
	e.exec("DROP ROUTINE IF EXISTS " + name)
}

func (e *pgEngine) tables(names []string) ([]Table, error) {
	var out []Table
	for _, name := range names {
		rows, err := e.query(`SELECT a.attname, format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
			WHERE a.attrelid = '` + e.schema + `.` + name + `'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`)
		if err != nil {
			return nil, err
		}
		t := Table{Schema: e.schema, Name: name}
		for _, r := range rows {
			t.Columns = append(t.Columns, Column{Name: r[0], Type: r[1]})
		}
		out = append(out, t)
	}
	return out, nil
}

// MySQL.

type myEngine struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	conn  *sql.Conn
	name  string
	admin *sql.DB
}

// openMySQL connects and makes a fresh database the session works in.
// collation is the database's default, which the variables of a routine
// take and which decides how they compare.
func openMySQL(t *testing.T, ctx context.Context, env, name, collation string) *myEngine {
	cfg := testConfig(t, env)
	admin, err := mysql.Open(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" CHARACTER SET utf8mb4 COLLATE "+collation); err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db, err := mysql.Open(ctx, cfg, map[string]string{"sql_mode": "'STRICT_ALL_TABLES,NO_AUTO_VALUE_ON_ZERO'"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &myEngine{t: t, ctx: ctx, db: db, conn: conn, name: name, admin: admin}
}

func (e *myEngine) close() {
	e.conn.Close()
	e.db.Close()
	e.admin.Exec("DROP DATABASE IF EXISTS " + e.name)
	e.admin.Close()
}

func (e *myEngine) exec(stmt string) error {
	_, err := e.conn.ExecContext(e.ctx, stmt)
	return err
}

func (e *myEngine) query(q string) ([][]string, error) { return sqlRows(e.ctx, e.conn, q) }

func (e *myEngine) function(expr string) ([][]string, error) { return e.query("SELECT " + expr) }

func (e *myEngine) procedure(name string, in []string, outs int) ([]string, error) {
	args := append([]string{}, in...)
	var vars []string
	for i := 0; i < outs; i++ {
		vars = append(vars, fmt.Sprintf("@o%d", i+1))
	}
	if err := e.exec("CALL " + name + "(" + strings.Join(append(args, vars...), ", ") + ")"); err != nil {
		return nil, err
	}
	if outs == 0 {
		return []string{}, nil
	}
	rows, err := e.query("SELECT " + strings.Join(vars, ", "))
	if err != nil {
		return nil, err
	}
	return rows[0], nil
}

func (e *myEngine) source(kind, name, table string) (string, error) {
	rows, err := e.conn.QueryContext(e.ctx, "SHOW CREATE "+kind+" "+name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return "", fmt.Errorf("SHOW CREATE %s %s returned nothing", kind, name)
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	for i, c := range cols {
		if c == "Create Function" || c == "Create Procedure" || c == "SQL Original Statement" {
			return string(vals[i]), nil
		}
	}
	return "", fmt.Errorf("SHOW CREATE %s %s has no definition column", kind, name)
}

func (e *myEngine) drop(kind, name, table string) { e.exec("DROP " + kind + " IF EXISTS " + name) }

func (e *myEngine) tables(names []string) ([]Table, error) {
	var out []Table
	for _, name := range names {
		rows, err := e.query("SELECT column_name, column_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '" + name + "' ORDER BY ordinal_position")
		if err != nil {
			return nil, err
		}
		t := Table{Schema: e.name, Name: name}
		for _, r := range rows {
			t.Columns = append(t.Columns, Column{Name: r[0], Type: r[1]})
		}
		out = append(out, t)
	}
	return out, nil
}

// Values.

const null = "<NULL>"

// sqlRows runs a query through database/sql and returns its rows as text.
func sqlRows(ctx context.Context, conn *sql.Conn, q string) ([][]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				row[i] = null
			case time.Time:
				row[i] = normalize(x.Format("2006-01-02 15:04:05.999999"))
			case []byte:
				row[i] = normalize(string(x))
			case float64:
				row[i] = normalize(strconv.FormatFloat(x, 'f', -1, 64))
			default:
				row[i] = normalize(fmt.Sprint(x))
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

var (
	numberRe    = regexp.MustCompile(`^-?([0-9]+\.?[0-9]*|\.[0-9]+)$`)
	timestampRe = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2}) ([0-9]{2}:[0-9]{2}:[0-9]{2})(\.[0-9]+)?(\+00)?$`)
)

// normalize writes a value in one spelling whichever database printed
// it. The three write the same number as 5, 5.00 or 5.0000 depending on
// the type it passed through, and the same instant with or without a
// zero fraction; neither difference is a difference in the value.
func normalize(s string) string {
	if numberRe.MatchString(s) {
		neg := strings.HasPrefix(s, "-")
		s = strings.TrimPrefix(s, "-")
		if strings.Contains(s, ".") {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		if strings.HasPrefix(s, ".") {
			s = "0" + s
		}
		s = strings.TrimLeft(s, "0")
		if s == "" || strings.HasPrefix(s, ".") {
			s = "0" + s
		}
		if neg && s != "0" {
			s = "-" + s
		}
		return s
	}
	if m := timestampRe.FindStringSubmatch(s); m != nil {
		frac := strings.TrimRight(m[3], "0")
		if frac == "." {
			frac = ""
		}
		return m[1] + " " + m[2] + frac
	}
	return s
}

// The comparison.

// step is one thing done to both databases.
type step struct {
	// fn is an expression that calls a function; its value is compared.
	fn string
	// proc calls a procedure with the arguments in and outs output
	// parameters after them; the output values are compared.
	proc string
	in   []string
	outs int
	// exec is a statement run for its effect, such as one that fires a
	// trigger. Whether it fails is compared.
	exec string
	// query is a query whose rows are compared.
	query string
}

func (s step) String() string {
	switch {
	case s.fn != "":
		return s.fn
	case s.proc != "":
		return fmt.Sprintf("%s(%s) + %d out", s.proc, strings.Join(s.in, ", "), s.outs)
	case s.query != "":
		return s.query
	}
	return s.exec
}

// run performs a step and describes the outcome. An error is an outcome
// like any other: a routine that rejects its input must do so on both
// sides.
func (s step) run(e engine) string {
	var rows [][]string
	var err error
	switch {
	case s.fn != "":
		rows, err = e.function(s.fn)
	case s.proc != "":
		var outs []string
		outs, err = e.procedure(s.proc, s.in, s.outs)
		rows = [][]string{outs}
	case s.query != "":
		rows, err = e.query(s.query)
	default:
		err = e.exec(s.exec)
	}
	if err != nil {
		return "ERROR"
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, strings.Join(r, " | "))
	}
	return strings.Join(lines, "\n")
}

// routineCase is one routine of a corpus.
type routineCase struct {
	kind, name, table string
	// ddl creates the routine on the source.
	ddl string
	// want is the status the converter must give it.
	want string
	// steps exercise it.
	steps []step
}

// corpus is everything a route's test needs.
type corpus struct {
	from, to string
	src, dst engine
	// tables are the fixture's tables, by their names on the source and
	// on the target.
	tables, targets []string
	// seed fills the tables; it is run on both sides before every case.
	seed  []string
	opt   Options
	cases []routineCase
	// create puts a routine on the source.
	create func(t *testing.T, c routineCase)
}

// dump returns the content of the fixture's tables.
func dump(e engine, tables []string) string {
	var b strings.Builder
	for _, t := range tables {
		rows, err := e.query("SELECT * FROM " + t)
		if err != nil {
			return "ERROR " + err.Error()
		}
		// Sorted here, so that no database's idea of order matters.
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = "  " + strings.Join(r, " | ") + "\n"
		}
		sort.Strings(lines)
		b.WriteString(strings.ToLower(t) + ":\n" + strings.Join(lines, ""))
	}
	return b.String()
}

// runCorpus is the test of a route: every routine marked converted must
// behave the same on both databases; those marked for review are run as
// well and the log says which of them do.
func runCorpus(t *testing.T, c corpus) {
	converted, verified, review, same := 0, 0, 0, 0
	for _, rc := range c.cases {
		t.Run(rc.name, func(t *testing.T) {
			for _, e := range []engine{c.src, c.dst} {
				tables := c.tables
				if e == c.dst {
					tables = c.targets
				}
				for _, table := range tables {
					if err := e.exec("DELETE FROM " + table); err != nil {
						t.Fatalf("emptying %s: %v", table, err)
					}
				}
				for _, stmt := range c.seed {
					if err := e.exec(stmt); err != nil {
						t.Fatalf("seeding: %v\n%s", err, stmt)
					}
				}
			}
			c.create(t, rc)
			defer c.src.drop(rc.kind, rc.name, rc.table)
			source, err := c.src.source(rc.kind, rc.name, rc.table)
			if err != nil {
				t.Fatal(err)
			}
			res := Convert(c.from, c.to, Object{Kind: rc.kind, Name: rc.name, Table: rc.table, Source: source}, c.opt)
			if res.Status != rc.want {
				t.Errorf("status is %s, want %s\nnotes: %s\nsource:\n%s\nresult:\n%s", res.Status, rc.want,
					strings.Join(res.Notes, "\n       "), source, strings.Join(res.Statements, ";\n"))
			}
			if res.Status == Unconverted {
				return
			}
			for _, stmt := range res.Statements {
				if err := c.dst.exec(stmt); err != nil {
					t.Fatalf("the converted statement fails on the target: %v\n%s", err, stmt)
				}
			}
			defer c.dst.drop(rc.kind, strings.ToLower(rc.name), strings.ToLower(rc.table))
			identical := true
			for _, s := range rc.steps {
				a, b := s.run(c.src), s.run(c.dst)
				if a != b {
					identical = false
					msg := fmt.Sprintf("%s\n  source: %s\n  target: %s", s, strings.ReplaceAll(a, "\n", "\n          "), strings.ReplaceAll(b, "\n", "\n          "))
					if res.Status == Converted {
						t.Errorf("converted, and behaves differently: %s", msg)
					} else {
						t.Logf("differs as the notes warn: %s", msg)
					}
				}
			}
			if a, b := dump(c.src, c.tables), dump(c.dst, c.targets); a != b {
				identical = false
				if res.Status == Converted {
					t.Errorf("converted, and leaves different data\nsource:\n%s\ntarget:\n%s", a, b)
				} else {
					t.Logf("leaves different data, as the notes warn\nsource:\n%s\ntarget:\n%s", a, b)
				}
			}
			if res.Status == Converted {
				converted++
				if identical {
					verified++
				}
			} else {
				review++
				if identical {
					same++
				}
				t.Logf("review (%s): %s", map[bool]string{true: "behaves identically", false: "behaves differently"}[identical], strings.Join(res.Notes, "; "))
			}
			if t.Failed() {
				t.Logf("converted code:\n%s", strings.Join(res.Statements, ";\n"))
			}
		})
	}
	t.Logf("%s to %s: %d routines; %d converted, of which %d verified identical; %d for review, of which %d behave identically",
		c.from, c.to, len(c.cases), converted, verified, review, same)
}
