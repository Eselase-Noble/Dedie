package oramy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// viewDrops removes everything the view fixture creates; errors for
// objects that are not there are ignored.
var viewDrops = []string{
	`DROP TRIGGER dedie_iw_sales_stock`,
	`DROP PROCEDURE dedie_iw_restock`,
	`DROP FUNCTION dedie_iw_vat`,
	`DROP PACKAGE dedie_iw_pricing`,
	`DROP VIEW dedie_iw_ranked_names`,
	`DROP VIEW dedie_iw_ranked`,
	`DROP VIEW dedie_iw_recent`,
	`DROP VIEW dedie_iw_late`,
	`DROP VIEW dedie_iw_code_names`,
	`DROP VIEW dedie_iw_code_len`,
	`DROP VIEW dedie_iw_a_best`,
	`DROP VIEW dedie_iw_sold`,
	`DROP VIEW dedie_iw_labels`,
	`DROP TABLE dedie_iw_sales PURGE`,
	`DROP TABLE dedie_iw_products PURGE`,
}

// The views of the fixture, one for each thing that can become of a view:
//
//	dedie_iw_labels, dedie_iw_sold   translate, and return the same
//	dedie_iw_a_best                  reads another view; its name sorts first
//	dedie_iw_code_len                MySQL accepts it, but does not count the blanks a CHAR is padded with
//	dedie_iw_code_names              reads the view that is dropped
//	dedie_iw_ranked, dedie_iw_recent use what has no translation
//	dedie_iw_late                    subtracts from a date, which MySQL takes for a number
//	dedie_iw_ranked_names            reads a view that is left out
var viewFixture = []string{
	`CREATE TABLE dedie_iw_products (id NUMBER(9) PRIMARY KEY, name VARCHAR2(50) NOT NULL, code CHAR(6) NOT NULL, price NUMBER(10,2) NOT NULL,
		stock NUMBER(9) NOT NULL, category VARCHAR2(20), note VARCHAR2(50))`,
	`CREATE TABLE dedie_iw_sales (id NUMBER(9) PRIMARY KEY, product_id NUMBER(9) NOT NULL, qty NUMBER(9) NOT NULL, sold DATE NOT NULL)`,
	`INSERT INTO dedie_iw_products VALUES (1, 'apple', 'AP', 1.50, 7, 'fruit', NULL)`,
	`INSERT INTO dedie_iw_products VALUES (2, 'pear', 'PEAR', 2.25, 0, 'fruit', 'ripe')`,
	`INSERT INTO dedie_iw_products VALUES (3, 'nail', 'N', 0.10, 1000, NULL, 'box of 100')`,
	`INSERT INTO dedie_iw_products VALUES (4, 'O''Hara tape', 'TAPE', 3.00, 5, 'tools', NULL)`,
	`INSERT INTO dedie_iw_sales VALUES (1, 1, 2, DATE '2024-03-01')`,
	`INSERT INTO dedie_iw_sales VALUES (2, 1, 1, DATE '2024-03-02')`,
	`INSERT INTO dedie_iw_sales VALUES (3, 3, 50, DATE '2024-03-02')`,
	`INSERT INTO dedie_iw_sales VALUES (4, 4, 1, DATE '2024-03-05')`,
	`CREATE VIEW dedie_iw_labels AS
		SELECT p.id, p.name || ' (' || NVL(p.category, 'none') || ')' AS label, -- NVL(a, b) || 'in a comment'
			DECODE(p.stock, 0, 'sold out', 'in stock') state, p.stock / 2 AS half, LENGTH(p.note) note_chars, p.note || '' AS note
		FROM dedie_iw_products p WHERE p.name <> 'NVL(x)' WITH READ ONLY`,
	`CREATE VIEW dedie_iw_sold AS SELECT p.id, p.name, NVL(SUM(s.qty), 0) AS sold, COUNT(s.id) AS sales
		FROM dedie_iw_products p LEFT JOIN dedie_iw_sales s ON s.product_id = p.id GROUP BY p.id, p.name`,
	`CREATE VIEW dedie_iw_a_best AS SELECT id, name, sold FROM dedie_iw_sold WHERE sold >= 3`,
	`CREATE VIEW dedie_iw_code_len AS SELECT id, LENGTH(code) AS code_len FROM dedie_iw_products`,
	`CREATE VIEW dedie_iw_code_names AS SELECT c.id, c.code_len FROM dedie_iw_code_len c`,
	`CREATE VIEW dedie_iw_ranked AS SELECT id, name, RANK() OVER (ORDER BY price DESC) AS place FROM dedie_iw_products`,
	`CREATE VIEW dedie_iw_recent AS SELECT id, TO_CHAR(sold, 'YYYY-MM') AS month FROM dedie_iw_sales`,
	`CREATE VIEW dedie_iw_late AS SELECT id FROM dedie_iw_sales WHERE sold > SYSDATE - 30`,
	`CREATE VIEW dedie_iw_ranked_names AS SELECT name FROM dedie_iw_ranked WHERE place = 1`,
	`CREATE FUNCTION dedie_iw_vat(amount NUMBER) RETURN NUMBER DETERMINISTIC IS
BEGIN
  RETURN amount * 1.2;
END;`,
	`CREATE PROCEDURE dedie_iw_restock(pid NUMBER) IS
BEGIN
  UPDATE dedie_iw_products SET stock = stock + 10 WHERE id = pid;
END;`,
	`CREATE PACKAGE dedie_iw_pricing AS
  FUNCTION net(amount NUMBER) RETURN NUMBER;
END dedie_iw_pricing;`,
	`CREATE PACKAGE BODY dedie_iw_pricing AS
  FUNCTION net(amount NUMBER) RETURN NUMBER IS
  BEGIN
    RETURN amount / 1.2;
  END;
END dedie_iw_pricing;`,
	`CREATE TRIGGER dedie_iw_sales_stock AFTER INSERT ON dedie_iw_sales FOR EACH ROW
BEGIN
  UPDATE dedie_iw_products SET stock = stock - :NEW.qty WHERE id = :NEW.product_id;
END;`,
}

// viewSetup prepares the view fixture in the source schema and a scratch
// database on the target. Both are removed when the test ends.
func viewSetup(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, src, dst *sql.DB) {
	t.Helper()
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_ORACLE_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("dedie_ov_%d", time.Now().UnixNano())
	admin, err := mysql.Open(ctx, dstAdmin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+db+" CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range viewDrops {
			src.ExecContext(bg, stmt)
		}
		src.Close()
		admin.ExecContext(bg, "DROP DATABASE IF EXISTS "+db)
		admin.Close()
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
	if dst, err = mysql.Open(ctx, dstCfg, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return srcCfg, dstCfg, src, dst
}

// viewCheck returns the preflight check of that name.
func viewCheck(t *testing.T, r *migrate.Report, name string) migrate.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("preflight has no %q check", name)
	return migrate.Check{}
}

// own keeps the lines that concern the view fixture. The source schema is
// migrated whole, and may hold more than this test put there.
func own(lines []string) string {
	var out []string
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "dedie_iw_") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "; ")
}

// targetViews lists the views of the target database.
func targetViews(t *testing.T, ctx context.Context, dst *sql.DB) string {
	t.Helper()
	var names sql.NullString
	if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(table_name ORDER BY table_name SEPARATOR ' ') FROM information_schema.views WHERE table_schema = DATABASE()`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	return names.String
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
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("preflight failed: %+v", report.Checks)
	}
	views := viewCheck(t, report, "Views")
	t.Logf("preflight Views: %s\n%s", views.Status, views.Detail)
	if views.Status != "warn" {
		t.Errorf("views check is %s, want a warning about the ones left out", views.Status)
	}
	for _, part := range []string{
		".DEDIE_IW_SOLD, ", ".DEDIE_IW_CODE_LEN", "view(s) are left out",
		".DEDIE_IW_RANKED: it calls RANK(), which this translator has no rule for",
		".DEDIE_IW_RECENT: it calls TO_CHAR(), which this translator has no rule for",
		".DEDIE_IW_RANKED_NAMES: it reads the view " + strings.ToUpper(srcCfg.Username) + ".DEDIE_IW_RANKED, which is left out",
		".DEDIE_IW_LATE: it adds to or subtracts from a date, which MySQL does not do with + and -",
	} {
		if !strings.Contains(views.Detail, part) {
			t.Errorf("views check lacks %q", part)
		}
	}
	if c := viewCheck(t, report, "Not migrated"); strings.HasPrefix(c.Detail, "views") || !strings.Contains(c.Detail, "packages") || !strings.Contains(c.Detail, "one script") {
		t.Errorf("not migrated: %s", c.Detail)
	}

	// The preview shows the views that will be attempted, each after the
	// view it reads, and none of those left out.
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, list)
	var previewed []string
	for _, s := range list {
		if s.Kind == "view" {
			previewed = append(previewed, s.Phase+" "+s.Name)
		}
	}
	if got, want := own(previewed), "cutover dedie_iw_sold; cutover dedie_iw_a_best; cutover dedie_iw_code_len; cutover dedie_iw_code_names; cutover dedie_iw_labels"; got != want {
		t.Errorf("previewed views:\n got %s\nwant %s", got, want)
	}
	// The comment and WITH READ ONLY are gone, the literal that looks like
	// a function call is untouched, and || takes NULL for an empty string.
	wantSQL := "CREATE VIEW `dedie_iw_labels` (`id`, `label`, `state`, `half`, `note_chars`, `note`) AS SELECT `p`.`id`, " +
		"NULLIF(CONCAT_WS('', `p`.`name`, ' (', COALESCE(`p`.`category`, 'none'), ')'), '') AS `label`, " +
		"CASE WHEN (`p`.`stock`) <=> (0) THEN 'sold out' ELSE 'in stock' END `state`, `p`.`stock` / 2 AS `half`, " +
		"CHAR_LENGTH(`p`.`note`) `note_chars`, NULLIF(CONCAT_WS('', `p`.`note`, NULL), '') AS `note` FROM `dedie_iw_products` `p` WHERE `p`.`name` <> 'NVL(x)'"
	if got := findStatement(t, list, "view", "dedie_iw_labels").SQL; got != wantSQL {
		t.Errorf("dedie_iw_labels:\n got %s\nwant %s", got, wantSQL)
	}

	// Before the job has run, what is not migrated is the views that could
	// not be translated and the PL/SQL, each with its definition.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "view DEDIE_IW_LATE; view DEDIE_IW_RANKED; view DEDIE_IW_RANKED_NAMES; view DEDIE_IW_RECENT; function DEDIE_IW_VAT; "+
		"procedure DEDIE_IW_RESTOCK; package DEDIE_IW_PRICING; package body DEDIE_IW_PRICING; trigger DEDIE_IW_SALES_STOCK"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	for _, want := range []struct{ kind, name, reason, source, end string }{
		{"view", "DEDIE_IW_RANKED", "it calls RANK()", "RANK() OVER (ORDER BY price DESC)", ""},
		{"view", "DEDIE_IW_RECENT", "it calls TO_CHAR()", "TO_CHAR(sold, 'YYYY-MM')", ""},
		{"view", "DEDIE_IW_LATE", "adds to or subtracts from a date", "sold > SYSDATE - 30", ""},
		{"function", "DEDIE_IW_VAT", "it is written in PL/SQL; converting stored code is switched off", "RETURN amount * 1.2;", "/"},
		{"procedure", "DEDIE_IW_RESTOCK", "it is written in PL/SQL", "SET stock = stock + 10 WHERE id = pid;", "/"},
		{"package", "DEDIE_IW_PRICING", "packages have no counterpart", "FUNCTION net(amount NUMBER) RETURN NUMBER;", "/"},
		{"package body", "DEDIE_IW_PRICING", "packages have no counterpart", "RETURN amount / 1.2;", "/"},
		{"trigger", "DEDIE_IW_SALES_STOCK", "PL/SQL", "AFTER INSERT ON dedie_iw_sales FOR EACH ROW", "/"},
	} {
		l := findLeftover(t, left, want.kind, want.name)
		if !strings.Contains(l.Reason, want.reason) || !strings.Contains(l.Source, want.source) || !strings.HasPrefix(l.Source, "CREATE OR REPLACE ") || l.End != want.end {
			t.Errorf("%s %s: reason %q, end %q, source %q", want.kind, want.name, l.Reason, l.End, l.Source)
		}
	}

	st, warnings := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "DEDIE_IW_SOLD=verified; DEDIE_IW_A_BEST=verified; DEDIE_IW_CODE_LEN=dropped; DEDIE_IW_CODE_NAMES=not created; DEDIE_IW_LABELS=verified"; got != want {
		t.Errorf("what became of the views:\n got %s\nwant %s", got, want)
	}
	if got, want := targetViews(t, ctx, dst), "dedie_iw_a_best dedie_iw_labels dedie_iw_sold"; got != want {
		t.Errorf("views on the target: %s, want %s", got, want)
	}
	// Four codes of six characters, blanks included, are 24 in Oracle.
	// MySQL gives a CHAR back without its padding and counts 2, 4, 1 and 4.
	for name, detail := range map[string]string{
		"DEDIE_IW_CODE_LEN":   "the values of column CODE_LEN add up to 24 on the source and 11 on the target",
		"DEDIE_IW_CODE_NAMES": ".DEDIE_IW_CODE_LEN, which was not kept",
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
	var chars sql.NullInt64
	var note sql.NullString
	if err := dst.QueryRowContext(ctx, "SELECT label, state, half, note_chars, note FROM dedie_iw_labels WHERE id = 1").Scan(&label, &state, &half, &chars, &note); err != nil {
		t.Fatal(err)
	}
	if label != "apple (fruit)" || state != "in stock" || !strings.HasPrefix(half, "3.5") || chars.Valid || note.Valid {
		t.Errorf("dedie_iw_labels for the apple: %q, %q, %s, %v, %v", label, state, half, chars, note)
	}
	var best string
	if err := dst.QueryRowContext(ctx, "SELECT GROUP_CONCAT(CONCAT(name, ':', sold) ORDER BY id) FROM dedie_iw_a_best").Scan(&best); err != nil {
		t.Fatal(err)
	}
	if best != "apple:3,nail:50" {
		t.Errorf("dedie_iw_a_best holds %s", best)
	}

	// Afterwards the views that were dropped again are handed over too.
	raw, _ := json.Marshal(st)
	left, err = pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); !strings.HasPrefix(got, "view DEDIE_IW_CODE_LEN; view DEDIE_IW_CODE_NAMES; view DEDIE_IW_LATE; view DEDIE_IW_RANKED; view DEDIE_IW_RANKED_NAMES; view DEDIE_IW_RECENT; function") {
		t.Errorf("not migrated after the job: %s", got)
	}
	if l := findLeftover(t, left, "view", "DEDIE_IW_CODE_LEN"); !strings.Contains(l.Reason, "not kept on the target") || !strings.Contains(l.Reason, "add up to 24") || !strings.Contains(l.Source, "LENGTH(code)") {
		t.Errorf("DEDIE_IW_CODE_LEN: reason %q, source %q", l.Reason, l.Source)
	}
}

// TestMigrationViewsTableScope: a migration of hand-picked tables takes a
// view along only when every table it reads is among them, says nothing
// about the others, and is told of its own tables' triggers only.
func TestMigrationViewsTableScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, Tables: []string{"dedie_iw_products"}}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := viewCheck(t, report, "Views"); c.Status != "ok" || !strings.Contains(c.Detail, "3 view(s) are translated") || strings.Contains(c.Detail, "left out") {
		t.Errorf("views check: %s: %s", c.Status, c.Detail)
	}
	left, err := Leftovers(ctx, srcCfg, dstCfg, cfg, State{})
	if err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); got != "" {
		t.Errorf("a job for one table without triggers leaves behind: %s", got)
	}
	cfg.Tables = []string{"dedie_iw_products", "dedie_iw_sales"}
	if left, err = Leftovers(ctx, srcCfg, dstCfg, cfg, State{}); err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); got != "trigger DEDIE_IW_SALES_STOCK" {
		t.Errorf("a job for the two tables leaves behind: %s", got)
	}

	cfg.Tables = []string{"dedie_iw_products"}
	st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "DEDIE_IW_CODE_LEN=dropped; DEDIE_IW_CODE_NAMES=not created; DEDIE_IW_LABELS=verified"; got != want {
		t.Errorf("what became of the views: %s, want %s", got, want)
	}
	if got := targetViews(t, ctx, dst); got != "dedie_iw_labels" {
		t.Errorf("views on the target: %s", got)
	}
}

// TestMigrationViewsExisting: a view the target already has under the name
// of one to be created stops the job, stays as it is, or is replaced,
// whichever was chosen.
func TestMigrationViewsExisting(t *testing.T) {
	const own = "CREATE VIEW dedie_iw_labels AS SELECT 'the target''s own' AS note"
	tables := []string{"dedie_iw_products", "dedie_iw_sales"}
	definition := func(ctx context.Context, dst *sql.DB) string {
		t.Helper()
		var def string
		if err := dst.QueryRowContext(ctx, `SELECT view_definition FROM information_schema.views WHERE table_schema = DATABASE() AND table_name = 'dedie_iw_labels'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}

	t.Run("refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
		mustExec(t, ctx, dst, own)
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, Tables: tables}
		report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c := viewCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "views: dedie_iw_labels") {
			t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
		}
		if len(report.Existing) != 1 || report.Existing[0] != (migrate.ExistingObject{Kind: "view", Name: "dedie_iw_labels"}) {
			t.Errorf("existing objects listed: %+v", report.Existing)
		}
		rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
		if _, err := Run(ctx, "views-refused", cfg, State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
			t.Errorf("the job ran: %v", err)
		}
		if got := targetViews(t, ctx, dst); got != "dedie_iw_labels" {
			t.Errorf("views on the target: %s", got)
		}
	})

	t.Run("skip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
		mustExec(t, ctx, dst, own)
		before := definition(ctx, dst)
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, Tables: tables, OnExisting: migrate.ExistingSkip}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Name == "dedie_iw_labels" || s.Kind == "drop" {
				t.Errorf("the preview of a job that keeps what is there has %s", previewLine(s))
			}
		}
		st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
		if got, want := viewResults(st), "DEDIE_IW_SOLD=verified; DEDIE_IW_A_BEST=verified; DEDIE_IW_CODE_LEN=dropped; DEDIE_IW_CODE_NAMES=not created"; got != want {
			t.Errorf("what became of the views: %s, want %s", got, want)
		}
		if after := definition(ctx, dst); after != before {
			t.Errorf("the target's own view changed:\nbefore %s\nafter  %s", before, after)
		}
		// The view that was kept is handed over with the rest: the
		// source's version of it did not arrive.
		raw, _ := json.Marshal(st)
		left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
		if err != nil {
			t.Fatal(err)
		}
		if l := findLeftover(t, left, "view", "DEDIE_IW_LABELS"); !strings.Contains(l.Reason, "already has a view or table of this name") {
			t.Errorf("DEDIE_IW_LABELS: %q", l.Reason)
		}
	})

	t.Run("replace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := viewSetup(t, ctx)
		mustExec(t, ctx, dst, own)
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, Tables: tables, OnExisting: migrate.ExistingReplace}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != "DROP VIEW `dedie_iw_labels`" {
			t.Errorf("the preview does not start by dropping the view: %+v", list[0])
		}
		st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
		if !strings.Contains(viewResults(st), "DEDIE_IW_LABELS=verified") {
			t.Errorf("what became of the views: %s", viewResults(st))
		}
		if def := definition(ctx, dst); !strings.Contains(def, "sold out") {
			t.Errorf("the view was not replaced: %s", def)
		}
	})
}
