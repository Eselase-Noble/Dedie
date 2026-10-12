package app

import (
	"strings"
	"testing"
	"time"

	"dedie/engine/migrate"
	"dedie/engine/store"
)

func attentionTitles(s Summary) []string {
	var out []string
	for _, a := range s.Attention {
		out = append(out, a.Level+": "+a.Title)
	}
	return out
}

func hasAttention(s Summary, part string) bool {
	for _, t := range attentionTitles(s) {
		if strings.Contains(t, part) {
			return true
		}
	}
	return false
}

func TestSummaryOfANewInstallation(t *testing.T) {
	f := newNotifyFixture(t)
	s, err := f.app.GetSummary(f.ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Connections) != 0 || len(s.Attention) != 0 {
		t.Errorf("an empty installation reports %+v", s)
	}
	if s.Setup == nil || *s.Setup != (SetupProgress{}) {
		t.Errorf("setup = %+v, want every first step still to do", s.Setup)
	}

	f.connection("shop", "postgres")
	s, _ = f.app.GetSummary(f.ctx, f.owner)
	if !s.Setup.Connection || s.Setup.Backup || s.Setup.Migration {
		t.Errorf("after the first connection, setup = %+v", *s.Setup)
	}
	if c := s.Connections[0]; c.Checked || c.Online {
		t.Errorf("a connection the monitor has not looked at yet is reported as %+v", c)
	}
}

func TestSummaryReportsHealthAndBackups(t *testing.T) {
	f := newNotifyFixture(t)
	up, down, flaky := f.connection("up", "postgres"), f.connection("down", "mysql"), f.connection("flaky", "postgres")
	docs := f.connection("docs", "mongodb")
	st := f.app.Store
	st.AddHealthSample(f.ctx, up.ID, true, 4, "")
	st.AddHealthSample(f.ctx, down.ID, false, 0, "dial tcp: connection refused")
	st.AddHealthSample(f.ctx, down.ID, false, 0, "dial tcp: connection refused")
	// One missed check after a good one is not an outage yet.
	st.AddHealthSample(f.ctx, flaky.ID, true, 3, "")
	st.AddHealthSample(f.ctx, flaky.ID, false, 0, "timeout")

	backup := func(conn store.Connection, id, status string) {
		t.Helper()
		b := store.Backup{ID: id, OrgID: f.owner.User.OrgID, ConnectionID: conn.ID, ConnectionName: conn.Name, Database: "shop",
			Kind: conn.Kind, Type: "full", Level: "full", Status: "running", CreatedBy: "test"}
		if err := st.CreateBackup(f.ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishBackup(f.ctx, id, status, map[string]string{"failed": "pg_dump: no space left on device"}[status], 10, "sum"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // backups are told apart by when they were made
	}
	backup(up, "bak_1", "completed")
	backup(up, "bak_2", "failed")
	backup(down, "bak_3", "failed")
	backup(down, "bak_4", "completed")

	s, err := f.app.GetSummary(f.ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ConnectionSummary{}
	for _, c := range s.Connections {
		by[c.Name] = c
	}
	if c := by["up"]; !c.Checked || !c.Online || c.LatencyMS != 4 || c.LastBackupAt.IsZero() || !c.BackupsApply {
		t.Errorf("up = %+v", c)
	}
	if c := by["down"]; !c.Checked || c.Online {
		t.Errorf("down = %+v", c)
	}
	if c := by[docs.Name]; c.BackupsApply {
		t.Error("MongoDB is reported as something Dedie backs up")
	}
	if !hasAttention(s, "error: down is not answering") {
		t.Errorf("the connection that is down is not reported: %v", attentionTitles(s))
	}
	if hasAttention(s, "flaky") {
		t.Errorf("one missed check was reported as an outage: %v", attentionTitles(s))
	}
	// Only a connection's latest backup counts: a failure followed by a
	// good backup is over.
	if !hasAttention(s, "The last backup of up failed") || hasAttention(s, "The last backup of down failed") {
		t.Errorf("failed backups reported: %v", attentionTitles(s))
	}
	if !s.Setup.Backup {
		t.Error("setup does not count the completed backups")
	}
	// What is broken comes first.
	for i, a := range s.Attention {
		if a.Level == LevelError && i > 0 && s.Attention[i-1].Level != LevelError {
			t.Errorf("an error is listed after a warning: %v", attentionTitles(s))
		}
	}
}

func TestSummaryWarnsAboutProductionWithoutASchedule(t *testing.T) {
	f := newNotifyFixture(t)
	prod := f.unreachable("billing", EnvProduction)
	f.unreachable("billing-dev", EnvDevelopment)
	s, _ := f.app.GetSummary(f.ctx, f.owner)
	if !hasAttention(s, "warning: No backup schedule covers billing") || hasAttention(s, "billing-dev") {
		t.Fatalf("attention = %v", attentionTitles(s))
	}
	if _, err := f.app.SaveSchedule(f.ctx, f.owner, "", ScheduleInput{Name: "nightly", ConnectionID: prod.ID, Cron: "0 2 * * *", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s, _ = f.app.GetSummary(f.ctx, f.owner)
	if hasAttention(s, "No backup schedule") {
		t.Errorf("still warned after a schedule was added: %v", attentionTitles(s))
	}
	if !s.Connections[0].Scheduled {
		t.Error("the connection is not shown as scheduled")
	}
}

func TestSummaryListsMigrationsWaitingForSomebody(t *testing.T) {
	f := newNotifyFixture(t)
	src, dst := f.connection("old", "postgres"), f.connection("new", "postgres")
	job := func(name string, fields map[string]any) {
		t.Helper()
		j, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: name, SourceID: src.ID, TargetID: dst.ID})
		if err == nil && fields != nil {
			err = f.app.Store.SetJob(f.ctx, j.ID, fields)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	existing := `{"ok":false,"existing":[{"kind":"table","name":"public.orders"},{"kind":"index","name":"public.orders_pkey"}]}`
	job("plain draft", nil)
	job("broken", map[string]any{"status": StatusFailed, "error": "replication slot is gone\nmore detail"})
	job("on hold", map[string]any{"status": StatusPaused})
	job("undecided", map[string]any{"preflight": existing})
	job("decided", map[string]any{"preflight": existing, "config": `{"on_existing":"` + migrate.ExistingSkip + `"}`})
	job("running", map[string]any{"status": StatusRunning})

	s, err := f.app.GetSummary(f.ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"error: Migration failed: broken", "warning: Migration paused: on hold", "warning: Migration waiting for a decision: undecided"}
	got := attentionTitles(s)
	if len(got) != len(want) {
		t.Fatalf("attention = %v, want %v", got, want)
	}
	for _, w := range want {
		if !hasAttention(s, w) {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	for _, a := range s.Attention {
		if !strings.HasPrefix(a.Link, "/migrations/detail/?id=mig_") {
			t.Errorf("%s links to %q", a.Title, a.Link)
		}
		if strings.Contains(a.Title, "broken") && a.Detail != "Replication slot is gone." {
			t.Errorf("the failure is described as %q", a.Detail)
		}
		if strings.Contains(a.Title, "undecided") && !strings.Contains(a.Detail, "2 objects") {
			t.Errorf("the decision is described as %q", a.Detail)
		}
	}
	if !s.Setup.Migration {
		t.Error("setup does not count the migrations")
	}
}

func TestSummaryShowsARoleOnlyWhatItMaySee(t *testing.T) {
	f := newNotifyFixture(t)
	open, secret := f.unreachable("open", EnvProduction), f.unreachable("secret", EnvProduction)
	if err := f.app.SetConnectionAccess(f.ctx, f.owner, secret.ID, ConnectionAccess{Restricted: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.Connection{open, secret} {
		f.app.Store.AddHealthSample(f.ctx, c.ID, false, 0, "refused")
		f.app.Store.AddHealthSample(f.ctx, c.ID, false, 0, "refused")
	}

	// An operator without a grant is told nothing of the restricted one.
	s, err := f.app.GetSummary(f.ctx, f.user("oli", RoleOperator))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Connections) != 1 || s.Connections[0].Name != "open" {
		t.Errorf("an operator sees connections %+v", s.Connections)
	}
	for _, title := range attentionTitles(s) {
		if strings.Contains(title, "secret") {
			t.Errorf("an operator without a grant is told: %s", title)
		}
	}
	if !hasAttention(s, "open is not answering") || !hasAttention(s, "No backup schedule covers open") {
		t.Errorf("an operator is not told about the connection it may use: %v", attentionTitles(s))
	}

	// A viewer may see connections and migrations, not backups, and is
	// not walked through setting anything up.
	v, err := f.app.GetSummary(f.ctx, f.user("vic", RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	if v.Setup != nil {
		t.Error("a viewer is offered the first-run guide")
	}
	if hasAttention(v, "backup") || hasAttention(v, "Backup") {
		t.Errorf("a viewer is told about backups: %v", attentionTitles(v))
	}
	if !hasAttention(v, "open is not answering") {
		t.Errorf("a viewer is not told a connection is down: %v", attentionTitles(v))
	}
	for _, a := range v.Attention {
		if a.Link == monitoringLink {
			t.Errorf("a viewer is sent to the monitoring page, which it may not open")
		}
	}
	if v.Connections[0].BackupsApply || v.Connections[0].Scheduled {
		t.Errorf("a viewer is given backup details: %+v", v.Connections[0])
	}
}
