package orapg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// viewDrops removes everything the view fixture creates; errors for
// objects that are not there are ignored.
var viewDrops = []string{
	`DROP TRIGGER dedie_iv_sales_stock`,
	`DROP PROCEDURE dedie_iv_restock`,
	`DROP FUNCTION dedie_iv_vat`,
	`DROP PACKAGE dedie_iv_pricing`,
	`DROP VIEW dedie_iv_ranked_names`,
	`DROP VIEW dedie_iv_ranked`,
	`DROP VIEW dedie_iv_recent`,
	`DROP VIEW dedie_iv_code_names`,
	`DROP VIEW dedie_iv_code_len`,
	`DROP VIEW dedie_iv_a_best`,
	`DROP VIEW dedie_iv_sold`,
	`DROP VIEW dedie_iv_labels`,
	`DROP TABLE dedie_iv_sales PURGE`,
	`DROP TABLE dedie_iv_products PURGE`,
}

// The views of the fixture, one for each thing that can become of a view:
//
//	dedie_iv_labels, dedie_iv_sold   translate, and return the same
//	dedie_iv_a_best                  reads another view; its name sorts first
//	dedie_iv_code_len                PostgreSQL accepts it, but does not count the blanks a CHAR is padded with
//	dedie_iv_code_names              reads the view that is dropped
//	dedie_iv_ranked, dedie_iv_recent use what has no translation
//	dedie_iv_ranked_names            reads a view that is left out
//
// dedie_iv_labels divides two whole numbers, which is where PostgreSQL
// would drop the remainder if the translation did not see to it.
var viewFixture = []string{
	`CREATE TABLE dedie_iv_products (id NUMBER(9) PRIMARY KEY, name VARCHAR2(50) NOT NULL, code CHAR(6) NOT NULL, price NUMBER(10,2) NOT NULL,
		stock NUMBER(9) NOT NULL, category VARCHAR2(20), note VARCHAR2(50))`,
	`CREATE TABLE dedie_iv_sales (id NUMBER(9) PRIMARY KEY, product_id NUMBER(9) NOT NULL, qty NUMBER(9) NOT NULL, sold DATE NOT NULL)`,
	`INSERT INTO dedie_iv_products VALUES (1, 'apple', 'AP', 1.50, 7, 'fruit', NULL)`,
	`INSERT INTO dedie_iv_products VALUES (2, 'pear', 'PEAR', 2.25, 0, 'fruit', 'ripe')`,
	`INSERT INTO dedie_iv_products VALUES (3, 'nail', 'N', 0.10, 1000, NULL, 'box of 100')`,
	`INSERT INTO dedie_iv_products VALUES (4, 'O''Hara tape', 'TAPE', 3.00, 5, 'tools', NULL)`,
	`INSERT INTO dedie_iv_sales VALUES (1, 1, 2, DATE '2024-03-01')`,
	`INSERT INTO dedie_iv_sales VALUES (2, 1, 1, DATE '2024-03-02')`,
	`INSERT INTO dedie_iv_sales VALUES (3, 3, 50, DATE '2024-03-02')`,
	`INSERT INTO dedie_iv_sales VALUES (4, 4, 1, DATE '2024-03-05')`,
	`CREATE VIEW dedie_iv_labels AS
		SELECT p.id, p.name || ' (' || NVL(p.category, 'none') || ')' AS label, -- NVL(a, b) || 'in a comment'
			DECODE(p.stock, 0, 'sold out', 'in stock') state, p.stock / 2 AS half, LENGTH(p.note) note_chars, p.note || '' AS note
		FROM dedie_iv_products p WHERE p.name <> 'NVL(x)' WITH READ ONLY`,
	`CREATE VIEW dedie_iv_sold AS SELECT p.id, p.name, NVL(SUM(s.qty), 0) AS sold, COUNT(s.id) AS sales
		FROM dedie_iv_products p LEFT JOIN dedie_iv_sales s ON s.product_id = p.id GROUP BY p.id, p.name`,
	`CREATE VIEW dedie_iv_a_best AS SELECT id, name, sold FROM dedie_iv_sold WHERE sold >= 3`,
	`CREATE VIEW dedie_iv_code_len AS SELECT id, LENGTH(code) AS code_len FROM dedie_iv_products`,
	`CREATE VIEW dedie_iv_code_names AS SELECT c.id, c.code_len FROM dedie_iv_code_len c`,
	`CREATE VIEW dedie_iv_ranked AS SELECT id, name, RANK() OVER (ORDER BY price DESC) AS place FROM dedie_iv_products`,
	`CREATE VIEW dedie_iv_recent AS SELECT id, TO_CHAR(sold, 'YYYY-MM') AS month FROM dedie_iv_sales`,
	`CREATE VIEW dedie_iv_ranked_names AS SELECT name FROM dedie_iv_ranked WHERE place = 1`,
	`CREATE FUNCTION dedie_iv_vat(amount NUMBER) RETURN NUMBER DETERMINISTIC IS
BEGIN
  RETURN amount * 1.2;
END;`,
	`CREATE PROCEDURE dedie_iv_restock(pid NUMBER) IS
BEGIN
  UPDATE dedie_iv_products SET stock = stock + 10 WHERE id = pid;
END;`,
	`CREATE PACKAGE dedie_iv_pricing AS
  FUNCTION net(amount NUMBER) RETURN NUMBER;
END dedie_iv_pricing;`,
	`CREATE PACKAGE BODY dedie_iv_pricing AS
  FUNCTION net(amount NUMBER) RETURN NUMBER IS
  BEGIN
    RETURN amount / 1.2;
  END;
END dedie_iv_pricing;`,
	`CREATE TRIGGER dedie_iv_sales_stock AFTER INSERT ON dedie_iv_sales FOR EACH ROW
BEGIN
  UPDATE dedie_iv_products SET stock = stock - :NEW.qty WHERE id = :NEW.product_id;
END;`,
}

// viewSetup prepares the view fixture in the source schema and a scratch
// database on the target. Both are removed when the test ends. The job is
// limited to the fixture's two tables wherever the whole schema is not
// what is being tested, so that nothing else in the schema matters.
func viewSetup(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, src *sql.DB, dst *pgx.Conn) {
	t.Helper()
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_ORACLE_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("dedie_ov_%d", time.Now().UnixNano())
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
		for _, stmt := range viewDrops {
			src.ExecContext(bg, stmt)
		}
		src.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	for _, stmt := range viewDrops {
		src.ExecContext(ctx, stmt)
	}
	for _, stmt := range viewFixture {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	dstCfg = dstAdmin
	dstCfg.Database = db
	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })
	return srcCfg, dstCfg, src, dst
}

// own keeps the lines that concern the view fixture. The source schema is
// migrated whole, and may hold more than this test put there.
func own(lines []string) string {
	var out []string
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "dedie_iv_") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "; ")
}

// targetViews lists the views of the target.
func targetViews(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var names string
	if err := dst.QueryRow(ctx, `SELECT coalesce(string_agg(viewname, ' ' ORDER BY viewname), '') FROM pg_views
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema') AND viewname LIKE 'dedie\_iv\_%'`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	return names
}

// levelReporter lets a test see the events of a job with their level.
type levelReporter struct {
	*memReporter
	on func(level, msg string)
}

func (l *levelReporter) Event(level, msg string) {
	l.on(level, msg)
	l.memReporter.Event(level, msg)
}

// runOffline runs a one-time copy to its end and returns its state and
// the warnings it reported.
func runOffline(t *testing.T, ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (State, []string) {
	t.Helper()
	var warnings []string
	rep := &levelReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(level, msg string) {
		if level == "warn" {
			warnings = append(warnings, msg)
		}
	}}
	out, err := Run(ctx, fmt.Sprintf("views-%d", time.Now().UnixNano()), cfg, State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("job: %v, %v", out, err)
	}
	_, st := rep.snapshot()
	return st, warnings
}

func viewResults(st State) string {
	var out []string
	for _, r := range st.Views {
		out = append(out, r.Name[strings.Index(r.Name, ".")+1:]+"="+r.Status)
	}
	return own(out)
}

func leftoverNames(list []migrate.Leftover) string {
	var out []string
	for _, l := range list {
		out = append(out, l.Kind+" "+l.Name[strings.Index(l.Name, ".")+1:])
	}
	return own(out)
}

func findLeftover(t *testing.T, list []migrate.Leftover, kind, name string) migrate.Leftover {
	t.Helper()
	for _, l := range list {
		if l.Kind == kind && strings.HasSuffix(l.Name, "."+name) {
			return l
		}
	}
	t.Fatalf("no %s %s among what is not migrated: %s", kind, name, leftoverNames(list))
	return migrate.Leftover{}
}

// TestMigrationViews follows the views of a schema through a migration:
// what preflight and the preview say will happen to them, what the job
// then does, and what the administrator is handed for the rest.
func TestMigrationViews(t *testing.T) {
	var _ migrate.LeftoverLister = pipeline{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("preflight failed: %+v", report.Checks)
	}
	views := findCheck(t, report, "Views")
	t.Logf("preflight Views: %s\n%s", views.Status, views.Detail)
	if views.Status != "warn" {
		t.Errorf("views check is %s, want a warning about the ones left out", views.Status)
	}
	for _, part := range []string{
		".DEDIE_IV_SOLD, ", ".DEDIE_IV_CODE_LEN", "view(s) are left out",
		".DEDIE_IV_RANKED: it calls RANK(), which this translator has no rule for",
		".DEDIE_IV_RECENT: it calls TO_CHAR(), which this translator has no rule for",
		".DEDIE_IV_RANKED_NAMES: it reads the view " + strings.ToUpper(srcCfg.Username) + ".DEDIE_IV_RANKED, which is left out",
	} {
		if !strings.Contains(views.Detail, part) {
			t.Errorf("views check lacks %q", part)
		}
	}
	if c := findCheck(t, report, "Not migrated"); strings.HasPrefix(c.Detail, "views") || !strings.Contains(c.Detail, "PL/SQL") || !strings.Contains(c.Detail, "one script") {
		t.Errorf("not migrated: %s", c.Detail)
	}

	// The preview shows the views that will be attempted, each after the
	// view it reads, and none of those left out.
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, list)
	schema := pgName(strings.ToUpper(srcCfg.Username))
	var previewed []string
	for _, s := range list {
		if s.Kind == "view" {
			previewed = append(previewed, s.Phase+" "+strings.TrimPrefix(s.Name, schema+"."))
		}
	}
	if got, want := own(previewed), "cutover dedie_iv_sold; cutover dedie_iv_a_best; cutover dedie_iv_code_len; cutover dedie_iv_code_names; cutover dedie_iv_labels"; got != want {
		t.Errorf("previewed views:\n got %s\nwant %s", got, want)
	}
	// The comment and WITH READ ONLY are gone, the literal that looks like
	// a function call is untouched, and the division is exact.
	q := func(name string) string { return `"` + schema + `"."` + name + `"` }
	wantSQL := `CREATE VIEW ` + q("dedie_iv_labels") + ` ("id", "label", "state", "half", "note_chars", "note") AS SELECT "p"."id", ` +
		`NULLIF(concat("p"."name", ' (', COALESCE("p"."category", 'none'), ')'), '') AS "label", ` +
		`CASE WHEN ("p"."stock") IS NOT DISTINCT FROM (0) THEN 'sold out' ELSE 'in stock' END "state", "p"."stock" / (2)::numeric AS "half", ` +
		`length("p"."note") "note_chars", NULLIF(concat("p"."note", NULL), '') AS "note" FROM ` + q("dedie_iv_products") + ` "p" WHERE "p"."name" <> 'NVL(x)'`
	if got := findStatement(t, list, "view", schema+".dedie_iv_labels").SQL; got != wantSQL {
		t.Errorf("dedie_iv_labels:\n got %s\nwant %s", got, wantSQL)
	}

	// Before the job has run, what is not migrated is the views that could
	// not be translated and the PL/SQL, each with its definition.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "view DEDIE_IV_RANKED; view DEDIE_IV_RANKED_NAMES; view DEDIE_IV_RECENT; function DEDIE_IV_VAT; "+
		"procedure DEDIE_IV_RESTOCK; package DEDIE_IV_PRICING; package body DEDIE_IV_PRICING; trigger DEDIE_IV_SALES_STOCK"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	for _, want := range []struct{ kind, name, reason, source, end string }{
		{"view", "DEDIE_IV_RANKED", "it calls RANK()", "RANK() OVER (ORDER BY price DESC)", ""},
		{"view", "DEDIE_IV_RECENT", "it calls TO_CHAR()", "TO_CHAR(sold, 'YYYY-MM')", ""},
		{"function", "DEDIE_IV_VAT", "PL/SQL is not translated", "RETURN amount * 1.2;", "/"},
		{"procedure", "DEDIE_IV_RESTOCK", "PL/SQL is not translated", "SET stock = stock + 10 WHERE id = pid;", "/"},
		{"package", "DEDIE_IV_PRICING", "PL/SQL is not translated", "FUNCTION net(amount NUMBER) RETURN NUMBER;", "/"},
		{"package body", "DEDIE_IV_PRICING", "PL/SQL is not translated", "RETURN amount / 1.2;", "/"},
		{"trigger", "DEDIE_IV_SALES_STOCK", "PL/SQL", "AFTER INSERT ON dedie_iv_sales FOR EACH ROW", "/"},
	} {
		l := findLeftover(t, left, want.kind, want.name)
		if !strings.Contains(l.Reason, want.reason) || !strings.Contains(l.Source, want.source) || !strings.HasPrefix(l.Source, "CREATE OR REPLACE ") || l.End != want.end {
			t.Errorf("%s %s: reason %q, end %q, source %q", want.kind, want.name, l.Reason, l.End, l.Source)
		}
	}

	st, warnings := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "DEDIE_IV_SOLD=verified; DEDIE_IV_A_BEST=verified; DEDIE_IV_CODE_LEN=dropped; DEDIE_IV_CODE_NAMES=dropped; DEDIE_IV_LABELS=verified"; got != want {
		t.Errorf("what became of the views:\n got %s\nwant %s", got, want)
	}
	if got, want := targetViews(t, ctx, dst), "dedie_iv_a_best dedie_iv_labels dedie_iv_sold"; got != want {
		t.Errorf("views on the target: %s, want %s", got, want)
	}
	// Four codes of six characters, blanks included, are 24 in Oracle.
	// PostgreSQL counts 2, 4, 1 and 4.
	for name, detail := range map[string]string{
		"DEDIE_IV_CODE_LEN":   "the values of column CODE_LEN add up to 24 on the source and 11 on the target",
		"DEDIE_IV_CODE_NAMES": ".DEDIE_IV_CODE_LEN, which was not kept",
	} {
		found := false
		for _, r := range st.Views {
			if strings.HasSuffix(r.Name, "."+name) {
				found = strings.Contains(r.Detail, detail)
			}
		}
		reported := false
		for _, w := range warnings {
			reported = reported || (strings.Contains(w, "."+name+" was not migrated") && strings.Contains(w, detail))
		}
		if !found || !reported {
			t.Errorf("%s: want %q in the job's state and in a warning; state %+v, warnings %q", name, detail, st.Views, warnings)
		}
	}
	// The views that were kept answer as the source's do: the halves keep
	// their fraction, and an empty string is NULL.
	var label, state, half string
	var chars *int
	var note *string
	if err := dst.QueryRow(ctx, `SELECT label, state, half::text, note_chars, note FROM `+q("dedie_iv_labels")+` WHERE id = 1`).Scan(&label, &state, &half, &chars, &note); err != nil {
		t.Fatal(err)
	}
	if label != "apple (fruit)" || state != "in stock" || !strings.HasPrefix(half, "3.5") || chars != nil || note != nil {
		t.Errorf("dedie_iv_labels for the apple: %q, %q, %s, %v, %v", label, state, half, chars, note)
	}
	var best string
	if err := dst.QueryRow(ctx, `SELECT string_agg(name || ':' || sold, ',' ORDER BY id) FROM `+q("dedie_iv_a_best")).Scan(&best); err != nil {
		t.Fatal(err)
	}
	if best != "apple:3,nail:50" {
		t.Errorf("dedie_iv_a_best holds %s", best)
	}

	// Afterwards the views that were dropped again are handed over too.
	raw, _ := json.Marshal(st)
	left, err = pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); !strings.HasPrefix(got, "view DEDIE_IV_CODE_LEN; view DEDIE_IV_CODE_NAMES; view DEDIE_IV_RANKED; view DEDIE_IV_RANKED_NAMES; view DEDIE_IV_RECENT; function") {
		t.Errorf("not migrated after the job: %s", got)
	}
	if l := findLeftover(t, left, "view", "DEDIE_IV_CODE_LEN"); !strings.Contains(l.Reason, "not kept on the target") || !strings.Contains(l.Reason, "add up to 24") || !strings.Contains(l.Source, "LENGTH(code)") {
		t.Errorf("DEDIE_IV_CODE_LEN: reason %q, source %q", l.Reason, l.Source)
	}
}

// TestMigrationViewsTableScope: a migration of hand-picked tables takes a
// view along only when every table it reads is among them, says nothing
// about the others, and is told of its own tables' triggers only.
func TestMigrationViewsTableScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Tables: []string{"dedie_iv_products"}}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Views"); c.Status != "ok" || !strings.Contains(c.Detail, "3 view(s) are translated") || strings.Contains(c.Detail, "left out") {
		t.Errorf("views check: %s: %s", c.Status, c.Detail)
	}
	left, err := Leftovers(ctx, srcCfg, dstCfg, cfg, State{})
	if err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); got != "" {
		t.Errorf("a job for one table without triggers leaves behind: %s", got)
	}
	cfg.Tables = []string{"dedie_iv_products", "dedie_iv_sales"}
	if left, err = Leftovers(ctx, srcCfg, dstCfg, cfg, State{}); err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); got != "trigger DEDIE_IV_SALES_STOCK" {
		t.Errorf("a job for the two tables leaves behind: %s", got)
	}

	cfg.Tables = []string{"dedie_iv_products"}
	st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "DEDIE_IV_CODE_LEN=dropped; DEDIE_IV_CODE_NAMES=dropped; DEDIE_IV_LABELS=verified"; got != want {
		t.Errorf("what became of the views: %s, want %s", got, want)
	}
	if got := targetViews(t, ctx, dst); got != "dedie_iv_labels" {
		t.Errorf("views on the target: %s", got)
	}
}

// TestMigrationViewsExisting: a view the target already has under the name
// of one to be created stops the job, stays as it is, or is replaced,
// whichever was chosen.
func TestMigrationViewsExisting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
	schema := pgName(strings.ToUpper(srcCfg.Username))
	name := `"` + schema + `"."dedie_iv_labels"`
	tables := []string{"dedie_iv_products", "dedie_iv_sales"}
	plant := func() {
		t.Helper()
		if _, err := dst.PgConn().Exec(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE; CREATE SCHEMA "`+schema+`";
			CREATE VIEW `+name+` AS SELECT 'the target''s own'::text AS note`).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	definition := func() string {
		t.Helper()
		var def string
		if err := dst.QueryRow(ctx, `SELECT pg_get_viewdef($1::regclass)`, name).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}

	// Refused: preflight names the view, and the job does not start.
	plant()
	cfg := migrate.Config{Mode: migrate.ModeOffline, Tables: tables}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "views: "+schema+".dedie_iv_labels") {
		t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
	}
	if got := existingList(report); got != "view "+schema+".dedie_iv_labels" {
		t.Errorf("existing objects listed: %s", got)
	}
	rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
	if _, err := Run(ctx, "views-refused", cfg, State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
		t.Errorf("the job ran: %v", err)
	}
	before := definition()

	// Kept: the target's view stays as it is, and the source's is handed
	// over with what is not migrated.
	cfg.OnExisting = migrate.ExistingSkip
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if strings.HasSuffix(s.Name, ".dedie_iv_labels") || s.Kind == "drop" {
			t.Errorf("the preview of a job that keeps what is there has %s", previewLine(s))
		}
	}
	st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "DEDIE_IV_SOLD=verified; DEDIE_IV_A_BEST=verified; DEDIE_IV_CODE_LEN=dropped; DEDIE_IV_CODE_NAMES=dropped"; got != want {
		t.Errorf("what became of the views: %s, want %s", got, want)
	}
	if after := definition(); after != before {
		t.Errorf("the target's own view changed:\nbefore %s\nafter  %s", before, after)
	}
	raw, _ := json.Marshal(st)
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if l := findLeftover(t, left, "view", "DEDIE_IV_LABELS"); !strings.Contains(l.Reason, "already has a view of this name") {
		t.Errorf("DEDIE_IV_LABELS: %q", l.Reason)
	}

	// Replaced: the target's view is dropped first and the source's
	// arrives in its place.
	plant()
	cfg.OnExisting = migrate.ExistingReplace
	if list, err = Preview(ctx, srcCfg, dstCfg, cfg); err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != `DROP VIEW IF EXISTS `+schema+`.dedie_iv_labels CASCADE` {
		t.Errorf("the preview does not start by dropping the view: %+v", list[0])
	}
	st, _ = runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if !strings.Contains(viewResults(st), "DEDIE_IV_LABELS=verified") {
		t.Errorf("what became of the views: %s", viewResults(st))
	}
	if def := definition(); !strings.Contains(def, "sold out") {
		t.Errorf("the view was not replaced: %s", def)
	}
}
