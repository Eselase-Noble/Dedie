package app

import (
	"testing"

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
