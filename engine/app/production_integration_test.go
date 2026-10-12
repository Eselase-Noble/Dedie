package app

import (
	"net/url"
	"os"
	"strconv"
	"testing"

	"dedie/engine/connector"
)

// TestProductionGuardAgainstADatabase runs the production safeguards
// against a real server, to see that a refused action leaves the data as it
// was and a confirmed one goes through. It needs a PostgreSQL server:
//
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
func TestProductionGuardAgainstADatabase(t *testing.T) {
	raw := os.Getenv("DEDIE_TEST_TARGET")
	if raw == "" {
		t.Skip("DEDIE_TEST_TARGET not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	password, _ := u.User.Password()
	f := newNotifyFixture(t)

	const database = "dedie_test_prodguard"
	in := ConnectionInput{Name: "admin", Kind: "postgres", Host: u.Hostname(), Port: port, Database: "postgres",
		Username: u.User.Username(), Password: password, SSLMode: "disable"}
	admin, err := f.app.SaveConnection(f.ctx, f.owner, "", in)
	if err != nil {
		t.Fatal(err)
	}
	run := func(actor Actor, conn, sql string) (QueryResponse, error) {
		return f.app.Query(f.ctx, actor, conn, QueryRequest{SQL: sql, Write: true})
	}
	run(f.owner, admin.ID, "DROP DATABASE IF EXISTS "+database)
	if _, err := run(f.owner, admin.ID, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run(f.owner, admin.ID, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)") })

	in.Name, in.Database, in.Environment = "ledger", database, EnvProduction
	prod, err := f.app.SaveConnection(f.ctx, f.owner, "", in)
	if err != nil {
		t.Fatal(err)
	}
	typed := f.confirming("ledger")
	// Creating and filling a table destroys nothing and asks nothing.
	if _, err := run(f.owner, prod.ID, "CREATE TABLE entries (id int PRIMARY KEY, note text); INSERT INTO entries VALUES (1, 'opening'), (2, 'rent')"); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		t.Helper()
		resp, err := f.app.Query(f.ctx, f.owner, prod.ID, QueryRequest{SQL: "SELECT count(*) FROM entries"})
		if err != nil {
			return "error: " + err.Error()
		}
		return *resp.Results[0].Rows[0][0]
	}

	one, note := "1", "corrected"
	edit := connector.RowChange{Schema: "public", Table: "entries", Action: "update", Key: map[string]*string{"id": &one}, Values: map[string]*string{"note": &note}}
	remove := connector.RowChange{Schema: "public", Table: "entries", Action: "delete", Key: map[string]*string{"id": &one}}
	if err := f.app.ChangeRow(f.ctx, f.owner, prod.ID, remove); !asksForName(err) {
		t.Fatalf("deleting a row without the name: %v", err)
	}
	if got := count(); got != "2" {
		t.Fatalf("the refused delete left %s rows, want 2", got)
	}
	if err := f.app.ChangeRow(f.ctx, typed, prod.ID, edit); err != nil {
		t.Errorf("editing a row with the name typed: %v", err)
	}
	if err := f.app.ChangeRow(f.ctx, typed, prod.ID, remove); err != nil {
		t.Errorf("deleting a row with the name typed: %v", err)
	}
	if got := count(); got != "1" {
		t.Errorf("%s rows after the confirmed delete, want 1", got)
	}

	for _, sql := range []string{"DELETE FROM entries", "TRUNCATE entries", "DROP TABLE entries"} {
		if _, err := run(f.owner, prod.ID, sql); !asksForName(err) {
			t.Errorf("%s without the name: %v", sql, err)
		}
	}
	if got := count(); got != "1" {
		t.Fatalf("the refused statements left %s rows, want 1", got)
	}
	if _, err := run(typed, prod.ID, "DROP TABLE entries"); err != nil {
		t.Errorf("DROP TABLE with the name typed: %v", err)
	}
	if got := count(); got == "1" {
		t.Error("the table is still there after the confirmed DROP")
	}

	// Ending sessions: refused without the name, carried out with it.
	if _, err := f.app.TerminateSessions(f.ctx, f.owner, prod.ID, connector.SessionFilter{Database: database}); !asksForName(err) {
		t.Errorf("disconnecting sessions without the name: %v", err)
	}
	if _, err := f.app.TerminateSessions(f.ctx, typed, prod.ID, connector.SessionFilter{Database: database}); err != nil {
		t.Errorf("disconnecting sessions with the name typed: %v", err)
	}
	// One entry for each action that went through, and none for a refusal.
	if n := len(f.audited("production.confirm")); n != 4 {
		t.Errorf("%d confirmations in the audit log, want 4", n)
	}
}
