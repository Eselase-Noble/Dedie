package store

import (
	"context"
	"testing"
)

func TestCopyProgressSumsTheTablesOfJobsInThePhase(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO orgs(id,name,created_at) VALUES('org1','Acme','2026-01-01T00:00:00Z'),('org2','Other','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, j := range []Job{
		{ID: "copying", OrgID: "org1"}, {ID: "streaming", OrgID: "org1"}, {ID: "empty", OrgID: "org1"}, {ID: "foreign", OrgID: "org2"},
	} {
		j.Name, j.Kind, j.SourceID, j.TargetID, j.Config, j.CreatedBy = j.ID, "postgres-postgres", "a", "b", []byte("{}"), "ada"
		if err := s.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		phase := "snapshot"
		if j.ID == "streaming" {
			phase = "streaming"
		}
		if err := s.SetJob(ctx, j.ID, map[string]any{"phase": phase}); err != nil {
			t.Fatal(err)
		}
	}
	tables := []JobTable{
		{Schema: "public", Table: "done", EstRows: 100},
		{Schema: "public", Table: "overrun", EstRows: 10},
		{Schema: "public", Table: "running", EstRows: 1000},
		{Schema: "public", Table: "waiting", EstRows: 500},
	}
	for _, id := range []string{"copying", "streaming", "foreign"} {
		if err := s.ResetJobTables(ctx, id, tables); err != nil {
			t.Fatal(err)
		}
	}
	// A finished table counts for what it really held, whatever was
	// estimated; an unfinished one that has passed its estimate counts
	// for what it has copied so far.
	for table, fields := range map[string]map[string]any{
		"done":    {"status": "copied", "rows_copied": 80},
		"overrun": {"status": "copying", "rows_copied": 25},
		"running": {"status": "copying", "rows_copied": 400},
	} {
		if err := s.SetJobTable(ctx, "copying", "public", table, fields); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.CopyProgress(ctx, "org1", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("jobs with progress: %v, want only the one that is copying and has tables", got)
	}
	want := CopyProgress{Tables: 4, TablesCopied: 1, RowsCopied: 505, EstRows: 1610, RowsExpected: 80 + 25 + 1000 + 500}
	if got["copying"] != want {
		t.Fatalf("progress = %+v, want %+v", got["copying"], want)
	}
}
