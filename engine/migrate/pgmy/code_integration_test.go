package pgmy

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
	"dedie/engine/migrate/pgpg"
)

// The stored code of the fixture, one piece for each thing that can
// become of it:
//
//	fee, deposit, accounts_audit   convert, and behave the same; the trigger
//	                 fires on two events and becomes two triggers in MySQL
//	ledger_stamp     converts; the test has the target refuse its second half
//	ratio            divides by a variable, which fails on 0 in PostgreSQL
//	                 and gives NULL in MySQL: needs review
//	tags             returns rows, which has no conversion
//
// The triggers are created before the rows, so the audit table holds what
// the source's triggers wrote; a trigger that fired on the target for the
// copied rows would write it all again.
const codeFixture = `
CREATE TABLE accounts (id int PRIMARY KEY, owner varchar(40) NOT NULL, balance numeric(12,2) NOT NULL, opened date NOT NULL);
CREATE TABLE audit (id serial PRIMARY KEY, account_id int NOT NULL, action varchar(10) NOT NULL, amount numeric(12,2));
CREATE TABLE ledger (id int PRIMARY KEY, note varchar(40), entries int);
CREATE FUNCTION fee(amount numeric) RETURNS numeric LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  RETURN round(amount * 0.015, 2);
END $$;
CREATE PROCEDURE deposit(acct int, amount numeric) LANGUAGE plpgsql AS $$
BEGIN
  UPDATE accounts SET balance = balance + amount WHERE id = acct;
END $$;
CREATE FUNCTION accounts_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    INSERT INTO audit (account_id, action, amount) VALUES (NEW.id, 'open', NEW.balance);
  ELSIF NEW.balance <> OLD.balance THEN
    INSERT INTO audit (account_id, action, amount) VALUES (NEW.id, 'change', NEW.balance - OLD.balance);
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER accounts_audit AFTER INSERT OR UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION accounts_audit();
CREATE FUNCTION ledger_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.entries := coalesce(NEW.entries, 0) + 1;
  RETURN NEW;
END $$;
CREATE TRIGGER ledger_stamp BEFORE INSERT OR UPDATE ON ledger FOR EACH ROW EXECUTE FUNCTION ledger_stamp();
CREATE FUNCTION ratio(a numeric, b numeric) RETURNS numeric LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  RETURN round(a / b, 2);
END $$;
CREATE FUNCTION tags(n int) RETURNS SETOF int LANGUAGE sql AS $$ SELECT generate_series(1, n) $$;
INSERT INTO accounts VALUES (1, 'ada', 100.00, '2024-01-05'), (2, 'bob', 20.50, '2024-02-11'), (3, 'cy', 0.00, '2024-03-01');
UPDATE accounts SET balance = balance + 5 WHERE id = 2;
INSERT INTO ledger VALUES (1, 'first', NULL);
`

// codeDatabases creates a source database with the code fixture and an
// empty target database, both dropped when the test ends.
func codeDatabases(t *testing.T, ctx context.Context) (srcCfg, dstCfg connector.Config, src *pgx.Conn, dst *sql.DB) {
	t.Helper()
	srcAdmin, dstCfg := testConfig(t, "DEDIE_TEST_SOURCE"), testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	db := fmt.Sprintf("dedie_d_%d", time.Now().UnixNano())
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
	srcCfg = srcAdmin
	srcCfg.Database, dstCfg.Database = db, db
	if src, err = postgres.Connect(ctx, srcCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		src.Close(bg)
		myAdmin.Exec("DROP DATABASE IF EXISTS " + db)
		myAdmin.Close()
		if c, err := postgres.Connect(bg, srcAdmin); err == nil {
			c.Exec(bg, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	if _, err := src.PgConn().Exec(ctx, codeFixture).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if dst, err = mysql.Open(ctx, dstCfg, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return srcCfg, dstCfg, src, dst
}

// targetCode lists the functions, procedures and triggers of the target
// database.
func targetCode(t *testing.T, ctx context.Context, dst *sql.DB) string {
	t.Helper()
	var names sql.NullString
	if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(name ORDER BY name SEPARATOR ' ') FROM (
		SELECT routine_name AS name FROM information_schema.routines WHERE routine_schema = DATABASE()
		UNION ALL SELECT CONCAT('trigger:', trigger_name) FROM information_schema.triggers WHERE trigger_schema = DATABASE()) x`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	return names.String
}

func codeResults(st pgpg.State) string {
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
func runCode(t *testing.T, ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config, during func()) (pgpg.State, []string) {
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
	out, err := Run(ctx, fmt.Sprintf("code-%d", time.Now().UnixNano()), cfg, pgpg.State{}, srcCfg, dstCfg, rep)
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
		"1 function(s), 1 procedure(s) and 2 trigger(s) are converted and created at the end",
		"function public.fee(amount numeric), procedure public.deposit(IN acct integer, IN amount numeric), trigger accounts_audit on public.accounts, trigger ledger_stamp on public.ledger",
		"1 need review and are not created", "function public.ratio(a numeric, b numeric)",
		"1 cannot be converted and are left for you", "function public.tags(n integer)",
		"What the conversion takes for granted:", "MySQL fires the triggers of a table in the order they were created",
	} {
		if !strings.Contains(check.Detail, part) {
			t.Errorf("stored code check lacks %q", part)
		}
	}

	// The preview ends with the code the job will create: routines, then
	// triggers, one for each event.
	list, err := Preview(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPhaseOrder(t, list)
	var previewed []string
	for _, s := range list {
		if s.Kind == "function" || s.Kind == "procedure" || s.Kind == "trigger" {
			previewed = append(previewed, s.Phase+" "+s.Kind+" "+s.Name+" "+s.End)
		}
	}
	if got, want := strings.Join(previewed, "; "), "cutover function fee //; cutover procedure deposit //; cutover trigger accounts_audit_ins //; cutover trigger accounts_audit_upd //; "+
		"cutover trigger ledger_stamp_ins //; cutover trigger ledger_stamp_upd //"; got != want {
		t.Errorf("previewed code:\n got %s\nwant %s", got, want)
	}

	// Before the job runs, what it will create is not left behind; the
	// routine for review comes with its draft and the notes.
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function public.ratio(a numeric, b numeric); function public.tags(n integer)"; got != want {
		t.Errorf("not migrated:\n got %s\nwant %s", got, want)
	}
	ratio := findLeftover(t, left, "function", "public.ratio(a numeric, b numeric)")
	if ratio.Status != "review" || len(ratio.Notes) == 0 || !strings.Contains(ratio.Draft, "DELIMITER //\nCREATE FUNCTION ratio(") || !strings.HasSuffix(ratio.Draft, "//\nDELIMITER ;") ||
		!strings.Contains(ratio.Source, "round(a / b, 2)") || !strings.Contains(ratio.Reason, "have to be checked by a person") {
		t.Errorf("ratio: %+v", ratio)
	}
	t.Logf("share notes: %q", ratio.Notes)
	tags := findLeftover(t, left, "function", "public.tags(n integer)")
	if tags.Status != "unconverted" || tags.Draft != "" || len(tags.Notes) == 0 || !strings.Contains(tags.Reason, "has no conversion") || !strings.Contains(tags.Source, "generate_series") {
		t.Errorf("tags: %+v", tags)
	}

	// While the job runs, a trigger of somebody else's appears under the
	// name the second half of ledger_stamp would take. The first half,
	// created by then, has to go again; the foreign trigger stays.
	st, warnings := runCode(t, ctx, srcCfg, dstCfg, cfg, func() {
		if _, err := dst.ExecContext(ctx, "CREATE TRIGGER ledger_stamp_upd BEFORE UPDATE ON audit FOR EACH ROW SET NEW.action = NEW.action"); err != nil {
			t.Error(err)
		}
	})
	if got, want := codeResults(st), "function public.fee(amount numeric)=created; procedure public.deposit(IN acct integer, IN amount numeric)=created; "+
		"trigger accounts_audit on public.accounts=created; trigger ledger_stamp on public.ledger=not created"; got != want {
		t.Errorf("what became of the code:\n got %s\nwant %s", got, want)
	}
	if got, want := targetCode(t, ctx, dst), "deposit fee trigger:accounts_audit_ins trigger:accounts_audit_upd trigger:ledger_stamp_upd"; got != want {
		t.Errorf("code on the target:\n got %s\nwant %s", got, want)
	}
	var onTable string
	if err := dst.QueryRowContext(ctx, `SELECT event_object_table FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND trigger_name = 'ledger_stamp_upd'`).Scan(&onTable); err != nil || onTable != "audit" {
		t.Errorf("the trigger that was not the job's is on %q now (%v)", onTable, err)
	}
	refused := false
	for _, w := range warnings {
		refused = refused || strings.Contains(w, "trigger ledger_stamp on public.ledger was not created on the target: the target has had a trigger called ledger_stamp_upd since the job started")
	}
	if !refused {
		t.Errorf("no warning about the trigger that was not created: %q", warnings)
	}
	if st.Objects == nil || st.Objects.Created != 3 || st.Objects.Review != 0 || st.Objects.Left != 3 {
		t.Errorf("summary: %+v", st.Objects)
	}

	// The triggers did not fire for the rows that were copied: the audit
	// table holds what the source's holds.
	srcAudit := func() string {
		var s string
		if err := src.QueryRow(ctx, `SELECT coalesce(string_agg(account_id || ':' || action || ':' || amount, ' ' ORDER BY id), '') FROM audit`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	dstAudit := func() string {
		var s sql.NullString
		if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(CONCAT(account_id, ':', action, ':', amount) ORDER BY id SEPARATOR ' ') FROM audit`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	if a, b := srcAudit(), dstAudit(); a != b || a != "1:open:100.00 2:open:20.50 3:open:0.00 2:change:5.00" {
		t.Errorf("audit after the copy:\nsource %s\ntarget %s", a, b)
	}

	// The same calls and the same changes on both sides give the same.
	var srcFee, dstFee string
	if err := src.QueryRow(ctx, `SELECT fee(1234.56)::text`).Scan(&srcFee); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT CAST(fee(1234.56) AS DECIMAL(12,2))`).Scan(&dstFee); err != nil {
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
		if _, err := src.Exec(ctx, stmt); err != nil {
			t.Fatalf("source: %v\n%s", err, stmt)
		}
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("target: %v\n%s", err, stmt)
		}
	}
	if a, b := srcAudit(), dstAudit(); a != b || !strings.HasSuffix(a, "1:change:50.00 4:open:7.25 3:change:-0.50 4:change:-0.50") {
		t.Errorf("audit after the same changes on both sides:\nsource %s\ntarget %s", a, b)
	}

	// Afterwards the trigger that was not created is handed over too.
	raw, _ := json.Marshal(st)
	left, err = pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function public.ratio(a numeric, b numeric); function public.tags(n integer); trigger ledger_stamp on public.ledger"; got != want {
		t.Errorf("not migrated after the job:\n got %s\nwant %s", got, want)
	}
	if l := findLeftover(t, left, "trigger", "ledger_stamp on public.ledger"); l.Status != "not created" || !strings.Contains(l.Reason, "since the job started") || strings.Count(l.Draft, "CREATE TRIGGER") != 2 {
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
	if c := findCheck(t, report, "Stored code"); !strings.Contains(c.Detail, "1 function(s), 0 procedure(s) and 0 trigger(s) are converted and created as well, as chosen") || !strings.Contains(c.Detail, "function public.ratio") {
		t.Errorf("stored code check: %s", c.Detail)
	}
	st, warnings := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
	if !strings.Contains(codeResults(st), "function public.ratio(a numeric, b numeric)=created (review)") || strings.Contains(codeResults(st), "tags") {
		t.Errorf("what became of the code: %s", codeResults(st))
	}
	if got, want := targetCode(t, ctx, dst), "deposit fee ratio trigger:accounts_audit_ins trigger:accounts_audit_upd trigger:ledger_stamp_ins trigger:ledger_stamp_upd"; got != want {
		t.Errorf("code on the target:\n got %s\nwant %s", got, want)
	}
	warned := false
	for _, w := range warnings {
		warned = warned || strings.Contains(w, "created function public.ratio(a numeric, b numeric), which needs review")
	}
	if !warned {
		t.Errorf("no warning that ratio needs review: %q", warnings)
	}
	if st.Objects == nil || st.Objects.Created != 4 || st.Objects.Review != 1 || st.Objects.Left != 1 {
		t.Errorf("summary: %+v", st.Objects)
	}
	// Where the note does not apply the two agree; and the trigger that
	// changes the row before it is stored does so on both sides.
	var a, b string
	if err := src.QueryRow(ctx, `SELECT ratio(10, 3)::text`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT CAST(ratio(10, 3) AS DECIMAL(12,2))`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a != b || a != "3.33" {
		t.Errorf("ratio(10, 3) is %s on the source and %s on the target", a, b)
	}
	for _, stmt := range []string{`INSERT INTO ledger VALUES (2, 'second', NULL)`, `UPDATE ledger SET note = 'again' WHERE id = 2`} {
		if _, err := src.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var srcEntries, dstEntries int
	if err := src.QueryRow(ctx, `SELECT entries FROM ledger WHERE id = 2`).Scan(&srcEntries); err != nil {
		t.Fatal(err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT entries FROM ledger WHERE id = 2`).Scan(&dstEntries); err != nil {
		t.Fatal(err)
	}
	if srcEntries != dstEntries || srcEntries != 2 {
		t.Errorf("ledger entries: %d on the source, %d on the target", srcEntries, dstEntries)
	}

	raw, _ := json.Marshal(st)
	left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := leftoverNames(left), "function public.ratio(a numeric, b numeric); function public.tags(n integer)"; got != want {
		t.Errorf("listed after the job: %s, want %s", got, want)
	}
	if l := findLeftover(t, left, "function", "public.ratio(a numeric, b numeric)"); l.Status != "created, needs review" || len(l.Notes) == 0 || l.Draft == "" {
		t.Errorf("ratio: %+v", l)
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
	if c := findCheck(t, report, "Stored code"); c.Status != "warn" || !strings.Contains(c.Detail, "switched off for this migration. 6 object(s) are left for you") {
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
	if len(left) != 6 {
		t.Errorf("not migrated: %s", leftoverNames(left))
	}
	if l := findLeftover(t, left, "function", "public.fee(amount numeric)"); l.Status != "" || l.Draft != "" || !strings.Contains(l.Reason, "switched off") || !strings.Contains(l.Source, "round(amount * 0.015, 2)") {
		t.Errorf("fee: %+v", l)
	}
}

// TestMigrationCodeTableScope: a migration of hand-picked tables converts
// the triggers of those tables and looks at nothing else.
func TestMigrationCodeTableScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
	cfg := migrate.Config{Mode: migrate.ModeOffline, Tables: []string{"public.ledger"}}
	report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, report, "Stored code"); !strings.Contains(c.Detail, "looks at the triggers of those tables only") || !strings.Contains(c.Detail, "0 function(s), 0 procedure(s) and 1 trigger(s) are converted") {
		t.Errorf("stored code check: %s", c.Detail)
	}
	st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
	if got := targetCode(t, ctx, dst); got != "trigger:ledger_stamp_ins trigger:ledger_stamp_upd" {
		t.Errorf("code on the target: %s (%s)", got, codeResults(st))
	}
}

// TestMigrationCodeExisting: a function the target already has under the
// name of one to be created stops the job, stays as it is, or is replaced,
// whichever was chosen.
func TestMigrationCodeExisting(t *testing.T) {
	const own = "CREATE FUNCTION fee(x INT) RETURNS INT DETERMINISTIC RETURN 42"
	answer := func(ctx context.Context, dst *sql.DB) string {
		t.Helper()
		var v string
		if err := dst.QueryRowContext(ctx, `SELECT fee(100)`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline}
		report, err := Preflight(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, report, "Existing objects"); report.OK || c.Status != "fail" || !strings.Contains(c.Detail, "functions: fee") {
			t.Errorf("ok=%v, existing objects %s: %s", report.OK, c.Status, c.Detail)
		}
		if got := existingList(report); got != "function fee" {
			t.Errorf("existing objects listed: %s", got)
		}
		rep := &memReporter{t: t, desired: "run", tables: map[string]map[string]any{}}
		if _, err := Run(ctx, "code-refused", cfg, pgpg.State{}, srcCfg, dstCfg, rep); err == nil || !strings.Contains(err.Error(), "Existing objects") {
			t.Errorf("the job ran: %v", err)
		}
	})

	t.Run("skip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingSkip}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Name == "fee" || s.Kind == "drop" {
				t.Errorf("the preview of a job that keeps what is there has %s", previewLine(s))
			}
		}
		st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
		if strings.Contains(codeResults(st), "fee") || !strings.Contains(codeResults(st), "procedure public.deposit(IN acct integer, IN amount numeric)=created") {
			t.Errorf("what became of the code: %s", codeResults(st))
		}
		if got := answer(ctx, dst); got != "42" {
			t.Errorf("the target's own function now answers %s", got)
		}
		raw, _ := json.Marshal(st)
		left, err := pipeline{}.Leftovers(ctx, srcCfg, dstCfg, cfg, raw)
		if err != nil {
			t.Fatal(err)
		}
		if l := findLeftover(t, left, "function", "public.fee(amount numeric)"); !strings.Contains(l.Reason, "already has a function called fee") || l.Draft == "" {
			t.Errorf("fee: %+v", l)
		}
	})

	t.Run("replace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		srcCfg, dstCfg, _, dst := codeDatabases(t, ctx)
		if _, err := dst.ExecContext(ctx, own); err != nil {
			t.Fatal(err)
		}
		cfg := migrate.Config{Mode: migrate.ModeOffline, OnExisting: migrate.ExistingReplace}
		list, err := Preview(ctx, srcCfg, dstCfg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 || list[0].Kind != "drop" || list[0].SQL != "DROP FUNCTION IF EXISTS `fee`" {
			t.Errorf("the preview does not start by dropping the function: %+v", list[0])
		}
		st, _ := runCode(t, ctx, srcCfg, dstCfg, cfg, nil)
		if !strings.Contains(codeResults(st), "function public.fee(amount numeric)=created") {
			t.Errorf("what became of the code: %s", codeResults(st))
		}
		if got := answer(ctx, dst); !strings.HasPrefix(got, "1.5") {
			t.Errorf("the function was not replaced: fee(100) is %s", got)
		}
	})
}
