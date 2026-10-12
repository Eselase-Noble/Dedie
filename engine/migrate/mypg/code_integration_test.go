package mypg

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
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

// The stored code of the fixture, one piece for each thing that can
// become of it:
//
//	fee, deposit, accounts_opened, accounts_changed   convert, and behave the same
//	ledger_stamp     converts; the test has the target refuse it
//	is_vip           compares text, which MySQL does without regard to case: needs review
//	remember         uses a session variable, which has no conversion
//
// The triggers are created before the rows, so the audit table holds what
// the source's triggers wrote; a trigger that fired on the target for the
// copied rows would write it all again.
var codeFixture = []string{
	`CREATE TABLE accounts (id INT NOT NULL PRIMARY KEY, owner VARCHAR(40) NOT NULL, balance DECIMAL(12,2) NOT NULL, opened DATE NOT NULL) ENGINE=InnoDB`,
	`CREATE TABLE audit (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, account_id INT NOT NULL, action VARCHAR(10) NOT NULL, amount DECIMAL(12,2)) ENGINE=InnoDB`,
	`CREATE TABLE ledger (id INT NOT NULL PRIMARY KEY, note VARCHAR(40), entries INT) ENGINE=InnoDB`,
	`CREATE FUNCTION fee(amount DECIMAL(12,2)) RETURNS DECIMAL(12,2) DETERMINISTIC RETURN ROUND(amount * 0.015, 2)`,
	`CREATE PROCEDURE deposit(IN acct INT, IN amount DECIMAL(12,2))
BEGIN
  UPDATE accounts SET balance = balance + amount WHERE id = acct;
END`,
	`CREATE TRIGGER accounts_opened AFTER INSERT ON accounts FOR EACH ROW
  INSERT INTO audit (account_id, action, amount) VALUES (NEW.id, 'open', NEW.balance)`,
	`CREATE TRIGGER accounts_changed AFTER UPDATE ON accounts FOR EACH ROW
BEGIN
  IF NEW.balance <> OLD.balance THEN
    INSERT INTO audit (account_id, action, amount) VALUES (NEW.id, 'change', NEW.balance - OLD.balance);
  END IF;
END`,
	`CREATE TRIGGER ledger_stamp BEFORE INSERT ON ledger FOR EACH ROW SET NEW.entries = COALESCE(NEW.entries, 0) + 1`,
	`CREATE FUNCTION is_vip(name VARCHAR(40)) RETURNS INT DETERMINISTIC RETURN name = 'ada'`,
	`CREATE PROCEDURE remember(IN v INT)
BEGIN
  SET @last = v;
END`,
	`INSERT INTO accounts VALUES (1, 'ada', 100.00, '2024-01-05'), (2, 'bob', 20.50, '2024-02-11'), (3, 'cy', 0.00, '2024-03-01')`,
	`UPDATE accounts SET balance = balance + 5 WHERE id = 2`,
	`INSERT INTO ledger VALUES (1, 'first', NULL)`,
}

// codeDatabases creates a source database with the code fixture and an
// empty target database, both dropped when the test ends.
func codeDatabases(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, src *sql.DB, dst *pgx.Conn) {
	t.Helper()
	srcCfg, dstAdmin := testConfig(t, "DEDIE_TEST_MYSQL_SOURCE"), testConfig(t, "DEDIE_TEST_TARGET")
	db := fmt.Sprintf("dedie_c_%d", time.Now().UnixNano())
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
	dstCfg = dstAdmin
	srcCfg.Database, dstCfg.Database = db, db
	if src, err = mysql.Open(ctx, srcCfg, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		src.Close()
		srcAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		srcAdmin.Close()
		if c, err := postgres.Connect(bg, dstAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	for _, stmt := range codeFixture {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	if dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close(context.Background()) })
	return srcCfg, dstCfg, src, dst
}

// targetCode lists the functions, procedures and triggers of the target's
// schema.
func targetCode(t *testing.T, ctx context.Context, dst *pgx.Conn) string {
	t.Helper()
	var names string
	if err := dst.QueryRow(ctx, `SELECT coalesce(string_agg(name, ' ' ORDER BY name), '') FROM (
		SELECT p.proname::text AS name FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
		UNION ALL SELECT 'trigger:' || g.tgname FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
			WHERE c.relnamespace = 'public'::regnamespace AND NOT g.tgisinternal) x`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	return names
}

func codeResults(st mymy.State) string {
	var out []string
	for _, r := range st.Code {
		s := r.Kind + " " + r.Name + "=" + r.Status
		if r.Review {
			s += " (review)"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// runCode runs a one-time copy to its end. during, when given, is called
// once the tables are in and validated, just before the stored code is
// created.
func runCode(t *testing.T, ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, during func()) (mymy.State, []string) {
	t.Helper()
	var warnings []string
	rep := &levelReporter{memReporter: &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}, on: func(level, msg string) {
		if level == "warn" {
			warnings = append(warnings, msg)
		}
		if during != nil && strings.HasPrefix(msg, "creating the converted functions") {
			during()
		}
	}}
	out, err := Run(ctx, fmt.Sprintf("code-%d", time.Now().UnixNano()), cfg, mymy.State{}, srcCfg, dstCfg, rep)
	if err != nil || out != migrate.Completed {
		t.Fatalf("job: %v, %v", out, err)
	}
	_, st := rep.snapshot()
	return st, warnings
}

// TestMigrationCode follows the stored code of a database through a
// migration with the default setting: what preflight and the preview say,
// what the job creates and whether it then behaves as on the source, and
// what the administrator is handed for the rest.
func TestMigrationCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := codeDatabases(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("preflight failed: %+v", report.Checks)
	}
	check := findCheck(t, report, "Stored code")
	t.Logf("preflight Stored code: %s\n%s", check.Status, check.Detail)
	for _, part := range []string{
		"1 function(s), 1 procedure(s) and 3 trigger(s) are converted and created at the end",
		"function fee, procedure deposit, trigger accounts_opened, trigger accounts_changed, trigger ledger_stamp",
		"1 need review and are not created", "function is_vip",
		"1 cannot be converted and are left for you", "procedure remember",
		"What the conversion takes for granted:", "MySQL does not fire triggers for rows changed by a cascading foreign key",
	} {
		if !strings.Contains(check.Detail, part) {
			t.Errorf("stored code check lacks %q", part)
		}
	}
	if check.Status != "warn" {
		t.Errorf("stored code check is %s, want a warning about what is not created", check.Status)
	}

	// The preview ends with the code the job will create: routines, then
	// triggers, each trigger after the function PostgreSQL keeps its code in.
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, list)
	var previewed []string
	for _, s := range list {
		if s.Kind == "function" || s.Kind == "procedure" || s.Kind == "trigger" {
			previewed = append(previewed, s.Phase+" "+s.Kind+" "+s.Name)
		}
	}
	if got, want := strings.Join(previewed, "; "), "cutover function public.fee; cutover procedure public.deposit; "+
		"cutover function public.accounts_opened_fn; cutover trigger accounts_opened; cutover function public.accounts_changed_fn; cutover trigger accounts_changed; "+
		"cutover function public.ledger_stamp_fn; cutover trigger ledger_stamp"; got != want {
		t.Errorf("previewed code:\n got %s\nwant %s", got, want)
	}

	// Before the job runs, what it will create is not left behind; the
	// routine for review comes with its draft and the notes.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function is_vip; procedure remember"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	vip := findLeftover(t, left, "function", "is_vip")
	if vip.Status != "review" || len(vip.Notes) == 0 || !strings.Contains(vip.Draft, "CREATE FUNCTION public.is_vip") ||
		!strings.Contains(vip.Source, "RETURN name = 'ada'") || !strings.Contains(vip.Reason, "have to be checked by a person") {
		t.Errorf("is_vip: %+v", vip)
	}
	t.Logf("is_vip notes: %q", vip.Notes)
	rem := findLeftover(t, left, "procedure", "remember")
	if rem.Status != "unconverted" || rem.Draft != "" || len(rem.Notes) == 0 || !strings.Contains(rem.Reason, "has no conversion: line") || !strings.Contains(rem.Source, "SET @last = v") {
		t.Errorf("remember: %+v", rem)
	}

	// While the job runs the ledger moves away under it, so that the
	// target refuses the trigger after it has taken the trigger's function.
	st, warnings := runCode(t, ctx, srcCfg, dstCfg, cfg, func() {
		if _, err := dst.Exec(ctx, `ALTER TABLE ledger RENAME TO ledger_moved`); err != nil {
			t.Error(err)
		}
	})
	if got, want := codeResults(st), "function fee=created; procedure deposit=created; trigger accounts_opened=created; trigger accounts_changed=created; trigger ledger_stamp=not created"; got != want {
		t.Errorf("what became of the code:\n got %s\nwant %s", got, want)
	}
	// Nothing of the refused trigger stays, and nothing that was not to
	// be created is there.
	if got, want := targetCode(t, ctx, dst), "accounts_changed_fn accounts_opened_fn deposit fee trigger:accounts_changed trigger:accounts_opened"; got != want {
		t.Errorf("code on the target:\n got %s\nwant %s", got, want)
	}
	refused := false
	for _, w := range warnings {
		refused = refused || (strings.Contains(w, "trigger ledger_stamp was not created on the target: the target did not accept the converted trigger") && strings.Contains(w, "ledger"))
	}
	if !refused {
		t.Errorf("no warning about the refused trigger: %q", warnings)
	}
	if st.Objects == nil || st.Objects.Created != 4 || st.Objects.Review != 0 || st.Objects.Left != 3 {
		t.Errorf("summary: %+v", st.Objects)
	}

	// The triggers did not fire for the rows that were copied: the audit
	// table holds what the source's holds.
	audit := func(q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}) string {
		var s sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT GROUP_CONCAT(CONCAT(account_id, ':', action, ':', amount) ORDER BY id SEPARATOR ' ') FROM audit`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	pgAudit := func() string {
		var s string
		if err := dst.QueryRow(ctx, `SELECT coalesce(string_agg(account_id || ':' || action || ':' || amount, ' ' ORDER BY id), '') FROM audit`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if a, b := audit(src), pgAudit(); a != b || a != "1:open:100.00 2:open:20.50 3:open:0.00 2:change:5.00" {
		t.Errorf("audit after the copy:\nsource %s\ntarget %s", a, b)
	}

	// The same calls and the same changes on both sides give the same.
	var srcFee, dstFee string
	if err := src.QueryRowContext(ctx, `SELECT fee(1234.56)`).Scan(&srcFee); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRow(ctx, `SELECT fee(1234.56)::text`).Scan(&dstFee); err != nil {
		t.Fatal(err)
	}
	if srcFee != dstFee || srcFee != "18.52" {
		t.Errorf("fee(1234.56) is %s on the source and %s on the target", srcFee, dstFee)
	}
	for _, stmt := range []string{
		`CALL deposit(1, 50)`,
		`INSERT INTO accounts VALUES (4, 'dee', 7.25, '2024-04-01')`,
		`UPDATE accounts SET owner = 'bobby' WHERE id = 2`,
		`UPDATE accounts SET balance = balance - 0.50 WHERE id >= 3`,
	} {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("source: %v\n%s", err, stmt)
		}
		if _, err := dst.Exec(ctx, stmt); err != nil {
			t.Fatalf("target: %v\n%s", err, stmt)
		}
	}
	if a, b := audit(src), pgAudit(); a != b || !strings.HasSuffix(a, "1:change:50.00 4:open:7.25 3:change:-0.50 4:change:-0.50") {
		t.Errorf("audit after the same changes on both sides:\nsource %s\ntarget %s", a, b)
	}

	// Afterwards the trigger the target refused is handed over too, with
	// the target's message and the draft.
	raw, _ := json.Marshal(st)
	left, err = pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function is_vip; procedure remember; trigger ledger_stamp"; got != want {
		t.Errorf("not migrated after the job:\n got %s\nwant %s", got, want)
	}
	if l := findLeftover(t, left, "trigger", "ledger_stamp"); l.Status != "not created" || !strings.Contains(l.Reason, "did not accept the converted trigger") || !strings.Contains(l.Draft, "CREATE TRIGGER") {
		t.Errorf("ledger_stamp: %+v", l)
	}
}

// TestMigrationCodeReview: told to, a job also creates the routines that
// need review, and says which they are. What has no conversion is still
// not created.
func TestMigrationCodeReview(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, src, dst := codeDatabases(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeReview}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Stored code"); !strings.Contains(c.Detail, "1 function(s), 0 procedure(s) and 0 trigger(s) are converted and created as well, as chosen") || !strings.Contains(c.Detail, "function is_vip") {
		t.Errorf("stored code check: %s", c.Detail)
	}
	st, warnings := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
	if !strings.Contains(codeResults(st), "function is_vip=created (review)") || strings.Contains(codeResults(st), "remember") {
		t.Errorf("what became of the code: %s", codeResults(st))
	}
	if got := targetCode(t, ctx, dst); !strings.Contains(got, "is_vip") || strings.Contains(got, "remember") {
		t.Errorf("code on the target: %s", got)
	}
	warned := false
	for _, w := range warnings {
		warned = warned || strings.Contains(w, "created function is_vip, which needs review")
	}
	if !warned {
		t.Errorf("no warning that is_vip needs review: %q", warnings)
	}
	if st.Objects == nil || st.Objects.Created != 5 || st.Objects.Review != 1 || st.Objects.Left != 1 {
		t.Errorf("summary: %+v", st.Objects)
	}
	// The note says what to check, and it is true: MySQL takes 'ADA' for
	// 'ada' and PostgreSQL does not.
	var a, b int
	if err := src.QueryRowContext(ctx, `SELECT is_vip('ADA')`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRow(ctx, `SELECT is_vip('ADA')::int`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	t.Logf("is_vip('ADA') is %d on the source and %d on the target", a, b)

	raw, _ := json.Marshal(st)
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function is_vip; procedure remember"; got != want {
		t.Errorf("listed after the job: %s, want %s", got, want)
	}
	if l := findLeftover(t, left, "function", "is_vip"); l.Status != "created, needs review" || len(l.Notes) == 0 || l.Draft == "" {
		t.Errorf("is_vip: %+v", l)
	}
}

// TestMigrationCodeOff: with conversion switched off nothing is created,
// and all of the code is handed over as the source defines it.
func TestMigrationCodeOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Code: migrate.CodeOff}

	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Stored code"); c.Status != "warn" || !strings.Contains(c.Detail, "switched off for this migration. 7 object(s) are left for you") {
		t.Errorf("stored code check: %s: %s", c.Status, c.Detail)
	}
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Kind == "function" || s.Kind == "procedure" || s.Kind == "trigger" {
			t.Errorf("the preview has %s", previewLine(s))
		}
	}
	st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
	if len(st.Code) != 0 || targetCode(t, ctx, dst) != "" {
		t.Errorf("created %s; on the target: %s", codeResults(st), targetCode(t, ctx, dst))
	}
	raw, _ := json.Marshal(st)
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function fee; function is_vip; procedure deposit; procedure remember; trigger accounts_opened; trigger accounts_changed; trigger ledger_stamp"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	if l := findLeftover(t, left, "function", "fee"); l.Status != "" || l.Draft != "" || !strings.Contains(l.Reason, "switched off") || !strings.Contains(l.Source, "ROUND") {
		t.Errorf("fee: %+v", l)
	}
}

// TestMigrationCodeExisting: a function the target already has under the
// name of one to be created stops the job, stays as it is, or is replaced,
// whichever was chosen.
func TestMigrationCodeExisting(t *testing.T) {
	const own = `CREATE FUNCTION fee(x text) RETURNS text LANGUAGE sql AS $$ SELECT 'the target''s own' $$`
	body := func(ctx context.Context, dst *pgx.Conn) string {
		t.Helper()
		var def string
		if err := dst.QueryRow(ctx, `SELECT string_agg(prosrc, ' | ') FROM pg_proc WHERE proname = 'fee'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}

	t.Run("refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline}
		report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "public.fee") {
			t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
		}
		if got := existingList(report); got != "function public.fee(x text)" {
			t.Errorf("existing objects listed: %s", got)
		}
		rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
		if _, err := Run(ctx, "code-refused", cfg, mymy.State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
			t.Errorf("the job ran: %v", err)
		}
	})

	t.Run("skip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Name == "public.fee" || s.Kind == "drop" {
				t.Errorf("the preview of a job that keeps what is there has %s", previewLine(s))
			}
		}
		st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
		if strings.Contains(codeResults(st), "fee") || !strings.Contains(codeResults(st), "procedure deposit=created") {
			t.Errorf("what became of the code: %s", codeResults(st))
		}
		if got := body(ctx, dst); !strings.Contains(got, "the target''s own") || strings.Contains(got, "|") {
			t.Errorf("the target's own function: %s", got)
		}
		raw, _ := json.Marshal(st)
		left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
		if err != nil {
			t.Fatal(err)
		}
		if l := findLeftover(t, left, "function", "fee"); !strings.Contains(l.Reason, "already has a function called public.fee") || l.Draft == "" {
			t.Errorf("fee: %+v", l)
		}
	})

	t.Run("replace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.Exec(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != `DROP ROUTINE IF EXISTS public.fee(x text) CASCADE` {
			t.Errorf("the preview does not start by dropping the function: %+v", list[0])
		}
		st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
		if !strings.Contains(codeResults(st), "function fee=created") {
			t.Errorf("what became of the code: %s", codeResults(st))
		}
		if got := body(ctx, dst); strings.Contains(got, "the target''s own") {
			t.Errorf("the function was not replaced: %s", got)
		}
	})
}
