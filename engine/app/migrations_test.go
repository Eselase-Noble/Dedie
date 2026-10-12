package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/store"
)

func TestListedMigrationsCarryCopyProgressOnlyDuringTheCopy(t *testing.T) {
	f := newNotifyFixture(t)
	src, dst := f.connection("old", "postgres"), f.connection("new", "postgres")
	create := func(name string) store.Job {
		t.Helper()
		j, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: name, SourceID: src.ID, TargetID: dst.ID})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	draft, copying := create("draft"), create("copying")
	if err := f.app.Store.ResetJobTables(f.ctx, copying.ID, []store.JobTable{
		{Schema: "public", Table: "orders", EstRows: 900},
		{Schema: "public", Table: "users", EstRows: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.SetJob(f.ctx, copying.ID, map[string]any{"phase": migrate.PhaseSnapshot}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.SetJobTable(f.ctx, copying.ID, "public", "users", map[string]any{"status": "copied", "rows_copied": 120}); err != nil {
		t.Fatal(err)
	}

	jobs, err := f.app.ListMigrations(f.ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if byID[draft.ID].Copy != nil {
		t.Errorf("a job that is not copying reports progress: %+v", byID[draft.ID].Copy)
	}
	want := store.CopyProgress{Tables: 2, TablesCopied: 1, RowsCopied: 120, EstRows: 1000, RowsExpected: 1020}
	if got := byID[copying.ID].Copy; got == nil || *got != want {
		t.Errorf("copy progress = %+v, want %+v", got, want)
	}

	// Past the copy the summary is dropped again.
	if err := f.app.Store.SetJob(f.ctx, copying.ID, map[string]any{"phase": migrate.PhaseStreaming}); err != nil {
		t.Fatal(err)
	}
	if jobs, err = f.app.ListMigrations(f.ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Copy != nil {
			t.Errorf("%s still reports copy progress after the copy", j.Name)
		}
	}
}

// leftBehind is a pipeline between two kinds of database that no real
// pipeline connects, which lists what it leaves behind without asking
// either database.
type leftBehind struct{ migrate.Pipeline }

func (leftBehind) Leftovers(_ context.Context, src, _ connector.Config, cfg migrate.Config, state json.RawMessage) ([]migrate.Leftover, error) {
	if len(state) > 2 {
		// A job that has run hands over the views it had to drop as well.
		return []migrate.Leftover{{Kind: "view", Name: "dropped", Reason: "it was not kept", Source: "CREATE VIEW dropped AS SELECT 1"}}, nil
	}
	return []migrate.Leftover{{Kind: "procedure", Name: src.Database + ".restock", Reason: "not translated", Source: "CREATE PROCEDURE restock() BEGIN END", End: "/"}}, nil
}

func TestMigrationLeftovers(t *testing.T) {
	migrate.Register("mysql", "mongodb", leftBehind{})
	f := newNotifyFixture(t)
	src, dst := f.connection("old", "mysql"), f.connection("new", "mongodb")
	job, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: "move", SourceID: src.ID, TargetID: dst.ID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.app.MigrationLeftovers(f.ctx, f.owner, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := migrate.Leftover{Kind: "procedure", Name: "shop.restock", Reason: "not translated", Source: "CREATE PROCEDURE restock() BEGIN END", End: "/"}
	if len(list) != 1 || !reflect.DeepEqual(list[0], want) {
		t.Errorf("leftovers = %+v, want %+v", list, want)
	}

	// The pipeline is handed the job's state, which is where it reads
	// what became of the views it created.
	if err := f.app.Store.SetJob(f.ctx, job.ID, map[string]any{"state": `{"views":[{"name":"dropped","status":"dropped"}]}`}); err != nil {
		t.Fatal(err)
	}
	if list, err = f.app.MigrationLeftovers(f.ctx, f.owner, job.ID); err != nil || len(list) != 1 || list[0].Name != "dropped" {
		t.Errorf("after the job ran: %+v, %v", list, err)
	}

	// Reading definitions is for those who may run migrations.
	if _, err := f.app.MigrationLeftovers(f.ctx, f.user("vera", "viewer"), job.ID); err == nil {
		t.Error("a viewer was given the definitions")
	}

	// A pipeline between two databases of one kind copies everything as
	// it is and has no such list.
	same, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: "copy", SourceID: f.connection("a", "postgres").ID, TargetID: f.connection("b", "postgres").ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.MigrationLeftovers(f.ctx, f.owner, same.ID); err == nil || !strings.Contains(err.Error(), "copy views and stored code as they are") {
		t.Errorf("postgres to postgres: %v", err)
	}
}

func TestMigrationCodeSettingIsChecked(t *testing.T) {
	f := newNotifyFixture(t)
	src, dst := f.connection("old", "mysql"), f.connection("new", "postgres")
	for setting, ok := range map[string]bool{"": true, "converted": true, "review": true, "off": true, "everything": false} {
		in := MigrationInput{Name: "move " + setting, SourceID: src.ID, TargetID: dst.ID}
		in.Config.Code = setting
		j, err := f.app.CreateMigration(f.ctx, f.owner, in)
		if ok != (err == nil) {
			t.Errorf("code %q: %v", setting, err)
		}
		if err == nil {
			var cfg migrate.Config
			_ = json.Unmarshal(j.Config, &cfg)
			if cfg.Code != setting {
				t.Errorf("code %q was stored as %q", setting, cfg.Code)
			}
		}
	}
}
