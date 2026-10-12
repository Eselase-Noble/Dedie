package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"dedie/engine/migrate"
	"dedie/engine/store"
)

// Summary is what the overview shows: each connection with its health and
// its last backup, what is waiting for somebody, and how far a new
// installation has got. Every part holds only what the actor's role may
// see elsewhere; a part the role may not see is left empty.
type Summary struct {
	Connections []ConnectionSummary `json:"connections"`
	Attention   []Attention         `json:"attention"`
	// Setup is given to those who can add connections, for the first-run
	// guide; nil for everyone else.
	Setup *SetupProgress `json:"setup,omitempty"`
}

// ConnectionSummary is one connection on the overview.
type ConnectionSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Environment string `json:"environment"`
	// Checked is false until the monitor has looked at the connection.
	Checked   bool      `json:"checked"`
	Online    bool      `json:"online"`
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	// The fields below are filled for roles that may see backups.
	// BackupsApply is false for the kinds of database Dedie does not back up.
	BackupsApply bool `json:"backups_apply"`
	// LastBackupAt is when the newest completed backup was taken; zero
	// when there is none.
	LastBackupAt time.Time `json:"last_backup_at"`
	// Scheduled says whether an enabled schedule backs the connection up.
	Scheduled bool `json:"scheduled"`
}

// Attention is one thing that is waiting for somebody.
type Attention struct {
	// Level is "error" for what is broken and "warning" for what is
	// waiting or at risk.
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Link is the page where it can be dealt with.
	Link string    `json:"link"`
	At   time.Time `json:"at"`
}

// SetupProgress says which first steps an installation has taken.
type SetupProgress struct {
	Connection bool `json:"connection"`
	Backup     bool `json:"backup"`
	Migration  bool `json:"migration"`
}

// backedUp reports whether Dedie takes backups of a kind of database.
func backedUp(kind string) bool { return kind == "postgres" || kind == "mysql" }

// healthFresh is how recent a check has to be to speak for the present.
const healthFresh = 5 * time.Minute

// GetSummary gathers the overview for the actor.
func (a *App) GetSummary(ctx context.Context, actor Actor) (Summary, error) {
	out := Summary{Connections: []ConnectionSummary{}, Attention: []Attention{}}
	org := actor.User.OrgID
	var conns []store.Connection
	if actor.Can(PermViewConnections) {
		var err error
		if conns, err = a.visibleConnections(ctx, actor); err != nil {
			return out, err
		}
	}
	visible := map[string]*ConnectionSummary{}
	now := time.Now()
	for _, c := range conns {
		s := ConnectionSummary{ID: c.ID, Name: c.Name, Kind: c.Kind, Environment: c.Environment}
		samples, err := a.Store.HealthSamples(ctx, c.ID, now.Add(-healthFresh))
		if err != nil {
			return out, err
		}
		if n := len(samples); n > 0 {
			last := samples[n-1]
			s.Checked, s.Online, s.LatencyMS, s.CheckedAt = true, last.Online, last.LatencyMS, last.Time
			// As the monitor does, one missed check is not yet an outage.
			if !last.Online && n > 1 && !samples[n-2].Online {
				link := "/connections/"
				if actor.Can(PermServerAdmin) {
					link = monitoringLink
				}
				out.Attention = append(out.Attention, Attention{Level: LevelError, Title: c.Name + " is not answering",
					Detail: sentence(firstLineOf(last.Error)), Link: link, At: last.Time})
			}
		}
		out.Connections = append(out.Connections, s)
	}
	for i := range out.Connections {
		visible[out.Connections[i].ID] = &out.Connections[i]
	}

	var jobs []store.Job
	if actor.Can(PermViewMigrations) {
		var err error
		if jobs, err = a.Store.ListJobs(ctx, org); err != nil {
			return out, err
		}
		for _, j := range jobs {
			if item, ok := migrationAttention(j); ok {
				out.Attention = append(out.Attention, item)
			}
		}
	}

	backups := 0
	if actor.Can(PermBackup) {
		n, items, err := a.backupSummary(ctx, org, conns, visible)
		if err != nil {
			return out, err
		}
		backups = n
		out.Attention = append(out.Attention, items...)
	}

	if actor.Can(PermManageConnections) {
		out.Setup = &SetupProgress{Connection: len(conns) > 0, Backup: backups > 0, Migration: len(jobs) > 0}
	}
	// What is broken comes before what is waiting, and the newest first.
	sort.SliceStable(out.Attention, func(i, k int) bool {
		x, y := out.Attention[i], out.Attention[k]
		if x.Level != y.Level {
			return x.Level == LevelError
		}
		return x.At.After(y.At)
	})
	return out, nil
}

// migrationAttention says whether a migration is waiting for somebody: it
// failed, it was paused, or preflight found objects on the target and
// nobody has said what to do with them.
func migrationAttention(j store.Job) (Attention, bool) {
	link := "/migrations/detail/?id=" + j.ID
	switch j.Status {
	case StatusFailed:
		return Attention{Level: LevelError, Title: "Migration failed: " + j.Name, Detail: sentence(firstLineOf(j.Error)), Link: link, At: j.UpdatedAt}, true
	case StatusPaused:
		return Attention{Level: LevelWarning, Title: "Migration paused: " + j.Name,
			Detail: "It keeps its place, and an online source keeps its change log, until it is resumed or aborted.", Link: link, At: j.UpdatedAt}, true
	case StatusDraft:
		var report migrate.Report
		var cfg migrate.Config
		if len(j.Preflight) == 0 || json.Unmarshal(j.Preflight, &report) != nil {
			return Attention{}, false
		}
		_ = json.Unmarshal(j.Config, &cfg)
		undecided := cfg.OnExisting == "" || cfg.OnExisting == migrate.ExistingFail
		if n := len(report.Existing); n > 0 && undecided {
			return Attention{Level: LevelWarning, Title: "Migration waiting for a decision: " + j.Name,
				Detail: fmt.Sprintf("The target already has %s it would create. Choose to skip or replace them, or clear them away.", plural(n, "object")),
				Link:   link, At: j.UpdatedAt}, true
		}
	}
	return Attention{}, false
}

// backupSummary fills in when each visible connection was last backed up
// and whether a schedule covers it, and returns the number of completed
// backups along with what about backups needs attention.
func (a *App) backupSummary(ctx context.Context, org string, conns []store.Connection, visible map[string]*ConnectionSummary) (int, []Attention, error) {
	var items []Attention
	list, err := a.Store.ListBackups(ctx, org)
	if err != nil {
		return 0, nil, err
	}
	completed := 0
	// The list is newest first, so the first backup met for a connection
	// is its latest.
	latest := map[string]bool{}
	for _, b := range list {
		if b.Status == "completed" {
			completed++
		}
		s := visible[b.ConnectionID]
		if s == nil {
			continue
		}
		if b.Status == "completed" && b.CreatedAt.After(s.LastBackupAt) {
			s.LastBackupAt = b.CreatedAt
		}
		if !latest[b.ConnectionID] && b.Status != "running" {
			latest[b.ConnectionID] = true
			if b.Status == "failed" {
				items = append(items, Attention{Level: LevelError, Title: "The last backup of " + s.Name + " failed",
					Detail: sentence(firstLineOf(b.Error)), Link: backupsLink, At: b.CreatedAt})
			}
		}
	}
	schedules, err := a.Store.ListSchedules(ctx, org)
	if err != nil {
		return 0, nil, err
	}
	for _, sc := range schedules {
		s := visible[sc.ConnectionID]
		if s == nil {
			continue
		}
		if sc.Enabled {
			s.Scheduled = true
		}
		if sc.Enabled && sc.LastStatus == "failed" {
			items = append(items, Attention{Level: LevelError, Title: "Backup schedule " + sc.Name + " could not run",
				Detail: sentence(firstLineOf(sc.LastError)), Link: backupsLink, At: sc.LastRunAt})
		}
	}
	archives, err := a.Store.WALArchives(ctx, org)
	if err != nil {
		return 0, nil, err
	}
	for _, w := range archives {
		if s := visible[w.ConnectionID]; s != nil && w.Enabled && w.LastError != "" {
			items = append(items, Attention{Level: LevelError, Title: "Log archiving for " + s.Name + " has stopped",
				Detail: sentence(firstLineOf(w.LastError)), Link: backupsLink, At: w.UpdatedAt})
		}
	}
	for _, c := range conns {
		s := visible[c.ID]
		s.BackupsApply = backedUp(c.Kind)
		if s.BackupsApply && c.Environment == EnvProduction && !s.Scheduled {
			items = append(items, Attention{Level: LevelWarning, Title: "No backup schedule covers " + c.Name,
				Detail: "It is a production connection. Add a schedule so that backups are taken and verified without anyone remembering to.",
				Link:   backupsLink, At: c.CreatedAt})
		}
	}
	return completed, items, nil
}
