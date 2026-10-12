package mypg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

// The views of the fixture, one for each thing that can become of a view:
//
//	product_labels, sales_by_product   translate, and return the same
//	a_best_sellers                     reads another view; its name sorts first
//	half_stock                         PostgreSQL accepts it, but divides integers differently
//	half_stock_names                   reads the view that is dropped
//	ranked, recent_sales               use what has no translation
//	ranked_names                       reads a view that is left out
var viewFixture = []string{
	`CREATE TABLE products (id INT NOT NULL PRIMARY KEY, name VARCHAR(50) NOT NULL, price DECIMAL(10,2) NOT NULL,
		stock INT NOT NULL, category VARCHAR(20), note VARCHAR(50)) ENGINE=InnoDB`,
	`CREATE TABLE sales (id INT NOT NULL PRIMARY KEY, product_id INT NOT NULL, qty INT NOT NULL, sold DATE NOT NULL) ENGINE=InnoDB`,
	`INSERT INTO products VALUES (1, 'apple', 1.50, 7, 'fruit', NULL), (2, 'pear', 2.25, 0, 'fruit', 'ripe'),
		(3, 'nail', 0.10, 1000, NULL, 'box of 100'), (4, 'O''Hara tape', 3.00, 5, 'tools', NULL)`,
	`INSERT INTO sales VALUES (1, 1, 2, '2024-03-01'), (2, 1, 1, '2024-03-02'), (3, 3, 50, '2024-03-02'), (4, 4, 1, '2024-03-05')`,
	`CREATE VIEW product_labels AS SELECT p.id, CONCAT(p.name, ' (', IFNULL(p.category, 'none'), ')') AS label,
		IF(p.stock > 0, 'in stock', 'sold out') AS state, p.price * p.stock AS worth, CHAR_LENGTH(p.note) AS note_chars FROM products p`,
	`CREATE VIEW sales_by_product AS SELECT p.id, p.name, COALESCE(SUM(s.qty), 0) AS sold, COUNT(s.id) AS sales
		FROM products p LEFT JOIN sales s ON s.product_id = p.id GROUP BY p.id, p.name`,
	`CREATE VIEW a_best_sellers AS SELECT id, name, sold FROM sales_by_product WHERE sold >= 3`,
	`CREATE VIEW half_stock AS SELECT id, stock / 2 AS half FROM products`,
	`CREATE VIEW half_stock_names AS SELECT h.id, h.half FROM half_stock h`,
	`CREATE VIEW ranked AS SELECT id, name, RANK() OVER (ORDER BY price DESC) AS place FROM products`,
	`CREATE VIEW recent_sales AS SELECT id FROM sales WHERE sold > NOW() - INTERVAL 30 DAY`,
	`CREATE VIEW ranked_names AS SELECT name FROM ranked WHERE place = 1`,
	`CREATE FUNCTION vat(amount DECIMAL(10,2)) RETURNS DECIMAL(10,2) DETERMINISTIC RETURN amount * 1.2`,
	`CREATE PROCEDURE restock(IN pid INT) BEGIN UPDATE products SET stock = stock + 10 WHERE id = pid; END`,
	`CREATE TRIGGER sales_stock AFTER INSERT ON sales FOR EACH ROW UPDATE products SET stock = stock - NEW.qty WHERE id = NEW.product_id`,
}

// viewDatabases creates a source database with the fixture and an empty
// target database, both dropped when the test ends.
func viewDatabases(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, dst *pgx.Conn) {
	t.Helper()
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	db := fmt.Sprintf("dedie_v_%d", time.Now().UnixNano())
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
	defer src.Close()
	for _, stmt := range viewFixture {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })
	return srcCfg, dstCfg, dst
}

// targetViews lists the views of the target's schema.
func targetViews(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var names string
	if err := dst.QueryRow(ctx, `SELECT coalesce(string_agg(viewname, ' ' ORDER BY viewname), '') FROM pg_views WHERE schemaname = 'public'`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	return names
}

// runOffline runs a one-time copy to its end and returns its state and
// the warnings it reported.
func runOffline(t *testing.T, ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (mymy.State, []string) {
	t.Helper()
	var warnings []string
	rep := &levelReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(level, msg string) {
		if level == "warn" {
			warnings = append(warnings, msg)
		}
	}}
	out, err := Run(ctx, fmt.Sprintf("views-%d", time.Now().UnixNano()), cfg, mymy.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("job: %v, %v", out, err)
	}
	_, st := rep.snapshot()
	return st, warnings
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

func viewResults(st mymy.State) string {
	var out []string
	for _, r := range st.Views {
		out = append(out, r.Name[strings.Index(r.Name, ".")+1:]+"="+r.Status)
	}
	return strings.Join(out, " ")
}

func leftoverNames(list []migrate.Leftover) string {
	var out []string
	for _, l := range list {
		out = append(out, l.Kind+" "+l.Name)
	}
	return strings.Join(out, "; ")
}

func findLeftover(t *testing.T, list []migrate.Leftover, kind, name string) migrate.Leftover {
	t.Helper()
	for _, l := range list {
		if l.Kind == kind && l.Name == name {
			return l
		}
	}
	t.Fatalf("no %s %s among what is not migrated: %s", kind, name, leftoverNames(list))
	return migrate.Leftover{}
}

// TestMigrationViews follows the views of a database through a migration:
// what preflight and the preview say will happen to them, what the job
// then does, and what the administrator is handed for the rest.
func TestMigrationViews(t *testing.T) {
	var _ migrate.LeftoverLister = pipeline{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, dst := viewDatabases(t, ctx)
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
		"5 view(s) are translated", ".sales_by_product, ", ".half_stock", "3 view(s) are left out",
		".ranked: it calls RANK(), which this translator has no rule for",
		".recent_sales: it uses INTERVAL",
		".ranked_names: it reads the view " + srcCfg.Database + ".ranked, which is left out",
	} {
		if !strings.Contains(views.Detail, part) {
			t.Errorf("views check lacks %q", part)
		}
	}
	if c := findCheck(t, report, "Not migrated"); strings.Contains(c.Detail, "views, stored") || !strings.Contains(c.Detail, "stored routines, triggers") {
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
	if got, want := strings.Join(previewed, "; "), "cutover sales_by_product; cutover a_best_sellers; cutover half_stock; cutover half_stock_names; cutover product_labels"; got != want {
		t.Errorf("previewed views:\n got %s\nwant %s", got, want)
	}
	wantSQL := `CREATE VIEW "public"."product_labels" ("id", "label", "state", "worth", "note_chars") AS SELECT "p"."id" AS "id", ` +
		`(("p"."name")::text || (' (')::text || (COALESCE("p"."category", 'none'))::text || (')')::text) AS "label", ` +
		`CASE WHEN ("p"."stock" > 0) THEN 'in stock' ELSE 'sold out' END AS "state", ("p"."price" * "p"."stock") AS "worth", ` +
		`char_length("p"."note") AS "note_chars" FROM "public"."products" "p"`
	if got := findStatement(t, list, "view", "product_labels").SQL; got != wantSQL {
		t.Errorf("product_labels:\n got %s\nwant %s", got, wantSQL)
	}

	// Before the job has run, what is not migrated is the views that could
	// not be translated and the stored code, each with its definition.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "view ranked; view ranked_names; view recent_sales; procedure restock; function vat; trigger sales_stock"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	for _, want := range []struct{ kind, name, reason, source string }{
		{"view", "ranked", "it calls RANK()", "rank() OVER"},
		{"view", "recent_sales", "it uses INTERVAL", "interval 30 day"},
		{"function", "vat", "procedural language", "RETURN amount * 1.2"},
		{"procedure", "restock", "procedural language", "UPDATE products SET stock = stock + 10"},
		{"trigger", "sales_stock", "procedural language", "AFTER INSERT ON `sales`"},
	} {
		l := findLeftover(t, left, want.kind, want.name)
		if !strings.Contains(l.Reason, want.reason) || !strings.Contains(l.Source, want.source) || !strings.HasPrefix(l.Source, "CREATE ") {
			t.Errorf("%s %s: reason %q, source %q", want.kind, want.name, l.Reason, l.Source)
		}
		if strings.Contains(l.Source, "DEFINER=") {
			t.Errorf("%s %s still names its definer: %s", want.kind, want.name, l.Source)
		}
	}

	st, warnings := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "sales_by_product=verified a_best_sellers=verified half_stock=dropped half_stock_names=not created product_labels=verified"; got != want {
		t.Errorf("what became of the views:\n got %s\nwant %s", got, want)
	}
	if got, want := targetViews(t, ctx, dst), "a_best_sellers product_labels sales_by_product"; got != want {
		t.Errorf("views on the target: %s, want %s", got, want)
	}
	// 7, 0, 1000 and 5 halve to 506 in MySQL and to 505 where the
	// remainder is dropped.
	for name, detail := range map[string]string{
		"half_stock":       "the values of column half add up to 506.0000 on the source and 505 on the target",
		"half_stock_names": "it reads the view " + srcCfg.Database + ".half_stock, which was not kept",
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
	// The views that were kept answer as the source's do.
	var label, state string
	var chars *int
	if err := dst.QueryRow(ctx, `SELECT label, state, note_chars FROM product_labels WHERE id = 3`).Scan(&label, &state, &chars); err != nil {
		t.Fatal(err)
	}
	if label != "nail (none)" || state != "in stock" || chars == nil || *chars != 10 {
		t.Errorf("product_labels for the nail: %q, %q, %v", label, state, chars)
	}
	var best string
	if err := dst.QueryRow(ctx, `SELECT string_agg(name || ':' || sold, ',' ORDER BY id) FROM a_best_sellers`).Scan(&best); err != nil {
		t.Fatal(err)
	}
	if best != "apple:3,nail:50" {
		t.Errorf("a_best_sellers holds %s", best)
	}

	// Afterwards the views that were dropped again are handed over too.
	raw, _ := json.Marshal(st)
	left, err = pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "view half_stock; view half_stock_names; view ranked; view ranked_names; view recent_sales; procedure restock; function vat; trigger sales_stock"; got != want {
		t.Errorf("not migrated after the job:\n got %s\nwant %s", got, want)
	}
	if l := findLeftover(t, left, "view", "half_stock"); !strings.Contains(l.Reason, "not kept on the target") || !strings.Contains(l.Reason, "add up to 506.0000") || !strings.Contains(l.Source, "/ 2") {
		t.Errorf("half_stock: reason %q, source %q", l.Reason, l.Source)
	}
}

// TestMigrationViewsTableScope: a migration of hand-picked tables takes a
// view along only when every table it reads is among them, and says
// nothing about the others.
func TestMigrationViewsTableScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, dst := viewDatabases(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Tables: []string{"products"}}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Views"); c.Status != "ok" || !strings.Contains(c.Detail, "3 view(s) are translated") || strings.Contains(c.Detail, "left out") {
		t.Errorf("views check: %s: %s", c.Status, c.Detail)
	}
	left, err := Leftovers(ctx, srcCfg, dstCfg, cfg, mymy.State{})
	if err != nil {
		t.Fatal(err)
	}
	if got := leftoverNames(left); got != "" {
		t.Errorf("a job for one table without triggers leaves behind: %s", got)
	}
	st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "half_stock=dropped half_stock_names=not created product_labels=verified"; got != want {
		t.Errorf("what became of the views: %s, want %s", got, want)
	}
	if got := targetViews(t, ctx, dst); got != "product_labels" {
		t.Errorf("views on the target: %s", got)
	}
}

// TestMigrationViewsExisting: a view the target already has under the name
// of one to be created stops the job, stays as it is, or is replaced,
// whichever was chosen.
func TestMigrationViewsExisting(t *testing.T) {
	const own = `CREATE VIEW product_labels AS SELECT 'the target''s own'::text AS note`
	definition := func(ctx context.Context, dst *pgx.Conn) string {
		var def string
		if err := dst.QueryRow(ctx, `SELECT pg_get_viewdef('product_labels'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}

	t.Run("refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, dst := viewDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline}
		report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "views: product_labels") {
			t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
		}
		if got := existingList(report); got != "view public.product_labels" {
			t.Errorf("existing objects listed: %s", got)
		}
		rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
		if _, err := Run(ctx, "views-refused", cfg, mymy.State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
			t.Errorf("the job ran: %v", err)
		}
		if got := targetViews(t, ctx, dst); got != "product_labels" {
			t.Errorf("views on the target: %s", got)
		}
	})

	t.Run("skip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, dst := viewDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		before := definition(ctx, dst)
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Name == "product_labels" || s.Kind == "drop" {
				t.Errorf("the preview of a job that keeps what is there has %s", previewLine(s))
			}
		}
		st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
		if got, want := viewResults(st), "sales_by_product=verified a_best_sellers=verified half_stock=dropped half_stock_names=not created"; got != want {
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
		if l := findLeftover(t, left, "view", "product_labels"); !strings.Contains(l.Reason, "already has a view of this name") {
			t.Errorf("product_labels: %q", l.Reason)
		}
	})

	t.Run("replace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, dst := viewDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != `DROP VIEW IF EXISTS public.product_labels CASCADE` {
			t.Errorf("the preview does not start by dropping the view: %+v", list[0])
		}
		st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
		if !strings.Contains(viewResults(st), "product_labels=verified") {
			t.Errorf("what became of the views: %s", viewResults(st))
		}
		if def := definition(ctx, dst); !strings.Contains(def, "in stock") {
			t.Errorf("the view was not replaced: %s", def)
		}
	})
}
