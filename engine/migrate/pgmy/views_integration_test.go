package pgmy

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
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// The views of the fixture, one for each thing that can become of a view:
//
//	product_labels, sales_by_product   translate, and return the same
//	a_best_sellers                     reads another view; its name sorts first
//	half_stock                         MySQL accepts it, but divides integers differently
//	half_stock_names                   reads the view that is dropped
//	ranked, recent_sales               use what has no translation
//	ranked_names                       reads a view that is left out
const viewFixture = `
CREATE TABLE products (id int PRIMARY KEY, name varchar(50) NOT NULL, price numeric(10,2) NOT NULL,
	stock int NOT NULL CONSTRAINT stock_not_negative CHECK (stock >= 0), category text, note varchar(50));
CREATE TABLE sales (id int PRIMARY KEY, product_id int NOT NULL, qty int NOT NULL, sold date NOT NULL);
INSERT INTO products VALUES (1, 'apple', 1.50, 7, 'fruit', NULL), (2, 'pear', 2.25, 0, 'fruit', 'ripe'),
	(3, 'nail', 0.10, 1000, NULL, 'box of 100'), (4, 'O''Hara tape', 3.00, 5, 'tools', NULL);
INSERT INTO sales VALUES (1, 1, 2, '2024-03-01'), (2, 1, 1, '2024-03-02'), (3, 3, 50, '2024-03-02'), (4, 4, 1, '2024-03-05');
CREATE VIEW product_labels AS SELECT p.id, p.name || ' (' || COALESCE(p.category, 'none') || ')' AS label,
	CASE WHEN p.stock > 0 THEN 'in stock' ELSE 'sold out' END AS state, p.price * p.stock AS worth, length(p.note) AS note_chars,
	p.category IN ('fruit', 'tools') AS known FROM products p WHERE p.name NOT LIKE 'zz%';
CREATE VIEW sales_by_product AS SELECT p.id, p.name, COALESCE(sum(s.qty), 0) AS sold, count(s.id) AS sales
	FROM products p LEFT JOIN sales s ON s.product_id = p.id GROUP BY p.id, p.name;
CREATE VIEW a_best_sellers AS SELECT id, name, sold FROM sales_by_product WHERE sold >= 3;
CREATE VIEW half_stock AS SELECT id, stock / 2 AS half FROM products;
CREATE VIEW half_stock_names AS SELECT h.id, h.half FROM half_stock h;
CREATE VIEW ranked AS SELECT id, name, rank() OVER (ORDER BY price DESC) AS place FROM products;
CREATE VIEW recent_sales AS SELECT id FROM sales WHERE sold > CURRENT_DATE - 30;
CREATE VIEW ranked_names AS SELECT name FROM ranked WHERE place = 1;
CREATE MATERIALIZED VIEW stock_snapshot AS SELECT id, stock FROM products;
CREATE FUNCTION vat(amount numeric) RETURNS numeric LANGUAGE sql IMMUTABLE AS $$ SELECT amount * 1.2 $$;
CREATE FUNCTION take_stock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	UPDATE products SET stock = stock - NEW.qty WHERE id = NEW.product_id;
	RETURN NEW;
END $$;
CREATE TRIGGER sales_stock AFTER INSERT ON sales FOR EACH ROW EXECUTE FUNCTION take_stock();
`

// viewDatabases creates a source database with the fixture and an empty
// target database, both dropped when the test ends.
func viewDatabases(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, dst *sql.DB) {
	t.Helper()
	srcAdmin, dstCfg := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	db := fmt.Sprintf("dedie_w_%d", time.Now().UnixNano())
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
	defer src.Close(ctx)
	if _, err := src.PgConn().Exec(ctx, viewFixture).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if dst, err = mysql.Open(ctx, dstCfg, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return srcCfg, dstCfg, dst
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
func runOffline(t *testing.T, ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (pgpg.State, []string) {
	t.Helper()
	var warnings []string
	rep := &levelReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(level, msg string) {
		if level == "warn" {
			warnings = append(warnings, msg)
		}
	}}
	out, err := Run(ctx, fmt.Sprintf("views-%d", time.Now().UnixNano()), cfg, pgpg.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("job: %v, %v", out, err)
	}
	_, st := rep.snapshot()
	return st, warnings
}

func viewResults(st pgpg.State) string {
	var out []string
	for _, r := range st.Views {
		out = append(out, r.Name+"="+r.Status)
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
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff}

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
		"5 view(s) are translated", "public.sales_by_product, public.a_best_sellers", "public.half_stock", "3 view(s) are left out",
		"public.ranked: it calls RANK(), which this translator has no rule for",
		"public.recent_sales: it adds to or subtracts from a date, which MySQL does not do with + and -",
		"public.ranked_names: it reads the view public.ranked, which is left out",
	} {
		if !strings.Contains(views.Detail, part) {
			t.Errorf("views check lacks %q", part)
		}
	}
	if c := findCheck(t, report, "Not migrated"); strings.HasPrefix(c.Detail, "views") || !strings.Contains(c.Detail, "functions, triggers") {
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
	// PostgreSQL stores the view with a cast on every literal and its own
	// spelling of IN and NOT LIKE; none of that reaches MySQL.
	wantSQL := "CREATE VIEW `product_labels` (`id`, `label`, `state`, `worth`, `note_chars`, `known`) AS SELECT `id`, " +
		"(CONCAT((CONCAT((CONCAT((CAST((`name`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin), ' (')), COALESCE(`category`, 'none'))), ')')) AS `label`, " +
		"CASE WHEN (`stock` > 0) THEN 'in stock' ELSE 'sold out' END AS `state`, (`price` * CAST((`stock`) AS DECIMAL(65,30))) AS `worth`, " +
		"CHAR_LENGTH((CAST((`note`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin)) AS `note_chars`, (`category` IN ('fruit', 'tools')) AS `known` " +
		"FROM `products` `p` WHERE ((CAST((`name`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin) NOT LIKE 'zz%')"
	if got := findStatement(t, list, "view", "product_labels").SQL; got != wantSQL {
		t.Errorf("product_labels:\n got %s\nwant %s", got, wantSQL)
	}

	// Before the job has run, what is not migrated is the views that could
	// not be translated and what PostgreSQL has that MySQL has not, each
	// with its definition.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "view public.ranked; view public.ranked_names; view public.recent_sales; "+
		"function public.take_stock(); function public.vat(amount numeric); check constraint public.products stock_not_negative; "+
		"materialized view public.stock_snapshot; trigger sales_stock on public.sales"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	for _, want := range []struct{ kind, name, reason, source string }{
		{"view", "public.ranked", "it calls RANK()", "rank() OVER (ORDER BY"},
		{"view", "public.recent_sales", "adds to or subtracts from a date", "CURRENT_DATE - 30"},
		{"function", "public.vat(amount numeric)", "not translated", "SELECT amount * 1.2"},
		{"function", "public.take_stock()", "not translated", "UPDATE products SET stock = stock - NEW.qty"},
		{"trigger", "sales_stock on public.sales", "not translated", "AFTER INSERT ON public.sales"},
		{"check constraint", "public.products stock_not_negative", "not translated", "stock >= 0"},
		{"materialized view", "public.stock_snapshot", "no materialized views", "CREATE MATERIALIZED VIEW"},
	} {
		l := findLeftover(t, left, want.kind, want.name)
		if !strings.Contains(l.Reason, want.reason) || !strings.Contains(l.Source, want.source) {
			t.Errorf("%s %s: reason %q, source %q", want.kind, want.name, l.Reason, l.Source)
		}
	}

	st, warnings := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "public.sales_by_product=verified public.a_best_sellers=verified public.half_stock=dropped public.half_stock_names=not created public.product_labels=verified"; got != want {
		t.Errorf("what became of the views:\n got %s\nwant %s", got, want)
	}
	if got, want := targetViews(t, ctx, dst), "a_best_sellers product_labels sales_by_product"; got != want {
		t.Errorf("views on the target: %s, want %s", got, want)
	}
	// 7, 0, 1000 and 5 halve to 505 where the remainder is dropped and to
	// 506 in MySQL.
	for name, detail := range map[string]string{
		"public.half_stock":       "the values of column half add up to 505 on the source and 506.0000 on the target",
		"public.half_stock_names": "it reads the view public.half_stock, which was not kept",
	} {
		found := false
		for _, r := range st.Views {
			if r.Name == name {
				found = strings.Contains(r.Detail, detail)
			}
		}
		reported := false
		for _, w := range warnings {
			reported = reported || (strings.Contains(w, name+" was not migrated") && strings.Contains(w, detail))
		}
		if !found || !reported {
			t.Errorf("%s: want %q in the job's state and in a warning; state %+v, warnings %q", name, detail, st.Views, warnings)
		}
	}
	// The views that were kept answer as the source's do.
	var label, state string
	var chars sql.NullInt64
	var known sql.NullInt64
	if err := dst.QueryRowContext(ctx, "SELECT label, state, note_chars, known FROM product_labels WHERE id = 3").Scan(&label, &state, &chars, &known); err != nil {
		t.Fatal(err)
	}
	if label != "nail (none)" || state != "in stock" || chars.Int64 != 10 || known.Valid {
		t.Errorf("product_labels for the nail: %q, %q, %v, %v", label, state, chars, known)
	}
	var best string
	if err := dst.QueryRowContext(ctx, "SELECT GROUP_CONCAT(CONCAT(name, ':', sold) ORDER BY id) FROM a_best_sellers").Scan(&best); err != nil {
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
	if got := leftoverNames(left); !strings.HasPrefix(got, "view public.half_stock; view public.half_stock_names; view public.ranked; view public.ranked_names; view public.recent_sales; function") {
		t.Errorf("not migrated after the job: %s", got)
	}
	if l := findLeftover(t, left, "view", "public.half_stock"); !strings.Contains(l.Reason, "not kept on the target") || !strings.Contains(l.Reason, "add up to 505") || !strings.Contains(l.Source, "/ 2") {
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
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, Tables: []string{"public.products"}}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Views"); c.Status != "ok" || !strings.Contains(c.Detail, "3 view(s) are translated") || strings.Contains(c.Detail, "left out") {
		t.Errorf("views check: %s: %s", c.Status, c.Detail)
	}
	st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
	if got, want := viewResults(st), "public.half_stock=dropped public.half_stock_names=not created public.product_labels=verified"; got != want {
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
	const own = "CREATE VIEW product_labels AS SELECT 'the target''s own' AS note"
	definition := func(ctx context.Context, dst *sql.DB) string {
		var def string
		if err := dst.QueryRowContext(ctx, `SELECT view_definition FROM information_schema.views WHERE table_schema = DATABASE() AND table_name = 'product_labels'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}

	t.Run("refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, dst := viewDatabases(t, ctx)
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff}
		report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "views: product_labels") {
			t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
		}
		if got := existingList(report); got != "view product_labels" {
			t.Errorf("existing objects listed: %s", got)
		}
		rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
		if _, err := Run(ctx, "views-refused", cfg, pgpg.State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
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
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		before := definition(ctx, dst)
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, OnExisting: migrate.ExistingSkip}
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
		if got, want := viewResults(st), "public.sales_by_product=verified public.a_best_sellers=verified public.half_stock=dropped public.half_stock_names=not created"; got != want {
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
		if l := findLeftover(t, left, "view", "public.product_labels"); !strings.Contains(l.Reason, "already has a view or table of this name") {
			t.Errorf("product_labels: %q", l.Reason)
		}
	})

	t.Run("replace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, dst := viewDatabases(t, ctx)
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff, OnExisting: migrate.ExistingReplace}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != "DROP VIEW IF EXISTS `product_labels`" {
			t.Errorf("the preview does not start by dropping the view: %+v", list[0])
		}
		st, _ := runOffline(t, ctx, srcCfg, dstCfg, cfg)
		if !strings.Contains(viewResults(st), "public.product_labels=verified") {
			t.Errorf("what became of the views: %s", viewResults(st))
		}
		if def := definition(ctx, dst); !strings.Contains(def, "in stock") {
			t.Errorf("the view was not replaced: %s", def)
		}
	})
}
