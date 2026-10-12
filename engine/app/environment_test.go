package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/store"
)

// unreachable is a connection to an address nothing listens on, so that an
// action which gets past the production check fails at once on connecting
// instead of doing anything.
func (f *notifyFixture) unreachable(name, env string) store.Connection {
	f.t.Helper()
	c, err := f.app.SaveConnection(f.ctx, f.owner, "", ConnectionInput{Name: name, Kind: "postgres", Host: "127.0.0.1", Port: 1,
		Database: "shop", Username: "dedie", Password: "secret", SSLMode: "disable", Environment: env})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// confirming is the owner having typed a connection's name.
func (f *notifyFixture) confirming(name string) Actor {
	a := f.owner
	a.Confirm = name
	return a
}

func (f *notifyFixture) audited(action string) []map[string]any {
	f.t.Helper()
	entries, err := f.app.Store.ListAudit(f.ctx, f.owner.User.OrgID, 0, 500)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range entries {
		if e.Action == action {
			var detail map[string]any
			json.Unmarshal([]byte(e.Detail), &detail)
			out = append(out, detail)
		}
	}
	return out
}

// asksForName reports whether err is the refusal that asks for a production
// connection's name.
func asksForName(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 428 && e.Code == CodeConfirmProduction
}

// reachedServer reports whether err came from trying to connect, which is
// as far as these tests let an allowed action go.
func reachedServer(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 502
}

func TestConnectionEnvironmentIsValidatedAndKept(t *testing.T) {
	f := newNotifyFixture(t)
	if _, err := f.app.SaveConnection(f.ctx, f.owner, "", ConnectionInput{Name: "x", Kind: "postgres", Host: "h", Database: "d", Username: "u", Password: "p", Environment: "prod"}); err == nil {
		t.Error("an unknown environment was accepted")
	}
	c := f.unreachable("billing", " Production ")
	if c.Environment != EnvProduction {
		t.Errorf("environment = %q, want it trimmed and lower-cased to production", c.Environment)
	}
	got, err := f.app.GetConnection(f.ctx, f.owner, c.ID)
	if err != nil || got.Environment != EnvProduction {
		t.Errorf("stored environment = %q (%v)", got.Environment, err)
	}
	if plain := f.connection("scratch", "postgres"); plain.Environment != "" {
		t.Errorf("a connection saved without an environment got %q", plain.Environment)
	}
}

func TestProductionLabelComesOffOnlyWithTheName(t *testing.T) {
	f := newNotifyFixture(t)
	c := f.unreachable("billing", EnvProduction)
	in := ConnectionInput{Name: "billing", Kind: "postgres", Host: "127.0.0.1", Port: 1, Database: "shop", Username: "dedie", SSLMode: "disable", Environment: EnvStaging}

	if _, err := f.app.SaveConnection(f.ctx, f.owner, c.ID, in); !asksForName(err) {
		t.Fatalf("relabelling production without its name: %v", err)
	}
	if _, err := f.app.SaveConnection(f.ctx, f.confirming("Billing"), c.ID, in); !asksForName(err) {
		t.Errorf("a name in the wrong case was accepted: %v", err)
	}
	// Changing anything else about it asks nothing.
	keep := in
	keep.Environment, keep.Host = EnvProduction, "127.0.0.2"
	if _, err := f.app.SaveConnection(f.ctx, f.owner, c.ID, keep); err != nil {
		t.Errorf("editing a production connection that stays production: %v", err)
	}
	got, err := f.app.SaveConnection(f.ctx, f.confirming("billing"), c.ID, in)
	if err != nil || got.Environment != EnvStaging {
		t.Fatalf("relabelling with the name typed: %q, %v", got.Environment, err)
	}
	if n := len(f.audited("production.confirm")); n != 1 {
		t.Errorf("%d confirmations in the audit log, want 1", n)
	}
	// It is no longer production, so the next change asks nothing.
	in.Environment = ""
	if _, err := f.app.SaveConnection(f.ctx, f.owner, c.ID, in); err != nil {
		t.Errorf("relabelling a staging connection: %v", err)
	}
}

func TestProductionAsksForItsNameBeforeAnythingDestructive(t *testing.T) {
	f := newNotifyFixture(t)
	prod := f.unreachable("billing", EnvProduction)
	row := connector.RowChange{Schema: "public", Table: "orders", Action: "delete", Key: map[string]*string{"id": nil}}

	actions := map[string]func(Actor, string) error{
		"edit a row": func(a Actor, id string) error { return f.app.ChangeRow(f.ctx, a, id, row) },
		"drop a table": func(a Actor, id string) error {
			_, err := f.app.Query(f.ctx, a, id, QueryRequest{SQL: "DROP TABLE orders", Write: true})
			return err
		},
		"delete everything": func(a Actor, id string) error {
			_, err := f.app.Query(f.ctx, a, id, QueryRequest{SQL: "delete from orders", Write: true})
			return err
		},
		"disconnect sessions": func(a Actor, id string) error {
			_, err := f.app.TerminateSessions(f.ctx, a, id, connector.SessionFilter{User: "app"})
			return err
		},
		"end a session": func(a Actor, id string) error { return f.app.CancelBackend(f.ctx, a, id, 42, true) },
		"drop a user":   func(a Actor, id string) error { return f.app.DropDatabaseAccount(f.ctx, a, id, "app", "", false) },
	}
	for name, do := range actions {
		if err := do(f.owner, prod.ID); !asksForName(err) {
			t.Errorf("%s on production without its name: %v", name, err)
		}
		if err := do(f.confirming("billing-old"), prod.ID); !asksForName(err) {
			t.Errorf("%s on production with another name: %v", name, err)
		}
		if err := do(f.confirming("billing"), prod.ID); !reachedServer(err) {
			t.Errorf("%s on production with its name did not go ahead: %v", name, err)
		}
	}
	if n := len(f.audited("production.confirm")); n != len(actions) {
		t.Errorf("%d confirmations in the audit log, want one for each of the %d actions", n, len(actions))
	}
	for _, detail := range f.audited("production.confirm") {
		if detail["connection"] != "billing" || detail["action"] == "" {
			t.Errorf("confirmation recorded as %v", detail)
		}
	}

	// The same things on a connection that is not production ask nothing.
	for _, env := range []string{"", EnvStaging, EnvDevelopment} {
		other := f.unreachable("other-"+env, env)
		for name, do := range actions {
			if err := do(f.owner, other.ID); !reachedServer(err) {
				t.Errorf("%s on a %q connection: %v", name, env, err)
			}
		}
	}
}

func TestProductionLeavesOrdinaryWorkAlone(t *testing.T) {
	f := newNotifyFixture(t)
	prod := f.unreachable("billing", EnvProduction)
	for _, q := range []QueryRequest{
		{SQL: "select * from orders"},
		{SQL: "drop table orders"}, // without writes allowed it runs read-only and the server refuses it
		{SQL: "update orders set status = 'paid' where id = 7", Write: true},
		{SQL: "insert into orders (id) values (1)", Write: true},
	} {
		if _, err := f.app.Query(f.ctx, f.owner, prod.ID, q); !reachedServer(err) {
			t.Errorf("%q (write %v) on production: %v", q.SQL, q.Write, err)
		}
	}
	// Cancelling a statement leaves the session connected.
	if err := f.app.CancelBackend(f.ctx, f.owner, prod.ID, 42, false); !reachedServer(err) {
		t.Errorf("cancelling a statement on production: %v", err)
	}
	if n := len(f.audited("production.confirm")); n != 0 {
		t.Errorf("%d confirmations recorded for work that needs none", n)
	}
}

func TestProductionCheckComesAfterTheOtherRules(t *testing.T) {
	f := newNotifyFixture(t)
	prod := f.unreachable("billing", EnvProduction)
	row := connector.RowChange{Schema: "public", Table: "orders", Action: "delete"}

	// Typing the name gives a role nothing it did not have.
	analyst := f.user("ann", RoleAnalyst)
	analyst.Confirm = "billing"
	if err := f.app.ChangeRow(f.ctx, analyst, prod.ID, row); !errors.Is(err, ErrForbidden) {
		t.Errorf("an analyst who typed the name edited a row: %v", err)
	}
	if _, err := f.app.Query(f.ctx, analyst, prod.ID, QueryRequest{SQL: "drop table orders", Write: true}); err == nil || asksForName(err) || reachedServer(err) {
		t.Errorf("an analyst who typed the name ran a write: %v", err)
	}

	// Nor does it open a read-only connection.
	ro := ConnectionInput{Name: "billing", Kind: "postgres", Host: "127.0.0.1", Port: 1, Database: "shop", Username: "dedie", SSLMode: "disable", Environment: EnvProduction, ReadOnly: true}
	if _, err := f.app.SaveConnection(f.ctx, f.owner, prod.ID, ro); err != nil {
		t.Fatal(err)
	}
	err := f.app.ChangeRow(f.ctx, f.confirming("billing"), prod.ID, row)
	var e *Error
	if !errors.As(err, &e) || e.Status != 403 {
		t.Errorf("a row of a read-only production connection was edited: %v", err)
	}

	// A restricted production connection stays hidden from those it is not
	// granted to: they are told it does not exist, not asked for its name.
	if err := f.app.SetConnectionAccess(f.ctx, f.owner, prod.ID, ConnectionAccess{Restricted: true}); err != nil {
		t.Fatal(err)
	}
	operator := f.user("oli", RoleOperator)
	if err := f.app.CancelBackend(f.ctx, operator, prod.ID, 1, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("a restricted production connection answered an operator without a grant with: %v", err)
	}
}

func TestMigrationIntoProductionAsksBeforeReplacing(t *testing.T) {
	f := newNotifyFixture(t)
	src, prod := f.connection("old", "postgres"), f.unreachable("billing", EnvProduction)
	create := func(name, onExisting string, fence bool) store.Job {
		t.Helper()
		j, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: name, SourceID: src.ID, TargetID: prod.ID,
			Config: migrate.Config{OnExisting: onExisting, FenceSource: fence}})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	// A migration that only adds to production starts as any other does.
	if err := f.app.MigrationAction(f.ctx, f.owner, create("adds", "", false).ID, "start"); err != nil {
		t.Errorf("starting a migration that replaces nothing: %v", err)
	}

	replacing := create("replaces", migrate.ExistingReplace, false)
	if err := f.app.MigrationAction(f.ctx, f.owner, replacing.ID, "start"); !asksForName(err) {
		t.Fatalf("starting a replacing migration into production without its name: %v", err)
	}
	if j, _ := f.app.Store.GetJob(f.ctx, replacing.OrgID, replacing.ID); j.Status != StatusDraft {
		t.Errorf("the refused migration is %s, want it still a draft", j.Status)
	}
	if err := f.app.MigrationAction(f.ctx, f.confirming("old"), replacing.ID, "start"); !asksForName(err) {
		t.Errorf("the source's name was taken for the target's: %v", err)
	}
	if err := f.app.MigrationAction(f.ctx, f.confirming("billing"), replacing.ID, "start"); err != nil {
		t.Fatalf("starting it with the name typed: %v", err)
	}
	if got := f.audited("production.confirm"); len(got) != 1 || got[0]["action"] != "migration.replace" {
		t.Errorf("confirmations recorded: %v", got)
	}

	// Once its objects exist there is nothing left to replace, and a
	// resume after a pause asks nothing.
	if err := f.app.Store.SetJob(f.ctx, replacing.ID, map[string]any{"status": StatusPaused, "state": `{"schema_done":true}`}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.MigrationAction(f.ctx, f.owner, replacing.ID, "resume"); err != nil {
		t.Errorf("resuming after the schema was created: %v", err)
	}
}

func TestCutoverThatFencesAProductionSourceAsksFirst(t *testing.T) {
	f := newNotifyFixture(t)
	prod, dst := f.unreachable("billing", EnvProduction), f.connection("new", "postgres")
	streaming := func(fence bool) store.Job {
		t.Helper()
		j, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: "m", SourceID: prod.ID, TargetID: dst.ID, Config: migrate.Config{FenceSource: fence}})
		if err == nil {
			err = f.app.Store.SetJob(f.ctx, j.ID, map[string]any{"status": StatusRunning, "phase": migrate.PhaseStreaming})
		}
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	if err := f.app.MigrationAction(f.ctx, f.owner, streaming(false).ID, "cutover"); err != nil {
		t.Errorf("a cutover that leaves the source alone: %v", err)
	}
	fenced := streaming(true)
	if err := f.app.MigrationAction(f.ctx, f.owner, fenced.ID, "cutover"); !asksForName(err) {
		t.Fatalf("a cutover that fences production, without its name: %v", err)
	}
	if err := f.app.MigrationAction(f.ctx, f.confirming("billing"), fenced.ID, "cutover"); err != nil {
		t.Errorf("the same cutover with the name typed: %v", err)
	}
}

func TestRestoreOverProductionAsksForTheConnectionToo(t *testing.T) {
	f := newNotifyFixture(t)
	prod := f.unreachable("billing", EnvProduction)
	b := store.Backup{ID: "bak_test", OrgID: f.owner.User.OrgID, ConnectionID: prod.ID, ConnectionName: prod.Name, Database: "shop",
		Kind: "postgres", Type: "full", Level: "full", Status: "completed", CreatedBy: "test", Path: "/nonexistent/shop.dediebak"}
	if err := f.app.Store.CreateBackup(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	over := RestoreInput{TargetID: prod.ID, Confirm: "shop"}
	if _, err := f.app.RestoreBackup(f.ctx, f.owner, b.ID, over, false); !asksForName(err) {
		t.Fatalf("restoring over production with only the database name: %v", err)
	}
	// With both names the restore goes on to look for the backup file,
	// which this test does not have.
	if _, err := f.app.RestoreBackup(f.ctx, f.confirming("billing"), b.ID, over, false); err == nil || asksForName(err) {
		t.Errorf("restoring over production with both names: %v", err)
	}
	// A new database on the same server overwrites nothing.
	beside := RestoreInput{TargetID: prod.ID, NewDatabase: "shop_copy", Confirm: "shop_copy"}
	if _, err := f.app.RestoreBackup(f.ctx, f.owner, b.ID, beside, false); err == nil || asksForName(err) {
		t.Errorf("restoring into a new database beside production: %v", err)
	}
	if got := f.audited("production.confirm"); len(got) != 1 || !strings.HasPrefix(got[0]["action"].(string), "backup.") {
		t.Errorf("confirmations recorded: %v", got)
	}
}
