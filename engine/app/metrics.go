package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// WriteMetrics writes the organization's state in the Prometheus text
// format. Values come from what the monitor and the workers have already
// recorded, so a scrape opens no database connections.
func (a *App) WriteMetrics(ctx context.Context, actor Actor, w io.Writer) error {
	if err := actor.require(PermServerAdmin); err != nil {
		return err
	}
	label := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
	metric := func(name, help, kind string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	conns, err := a.visibleConnections(ctx, actor)
	if err != nil {
		return err
	}
	type sample struct {
		name, kind string
		up         int
		latency    int64
	}
	var samples []sample
	for _, c := range conns {
		recent, err := a.Store.HealthSamples(ctx, c.ID, time.Now().Add(-5*time.Minute))
		if err != nil {
			return err
		}
		if len(recent) == 0 {
			continue // not checked yet; report nothing rather than a guess
		}
		last := recent[len(recent)-1]
		s := sample{name: c.Name, kind: c.Kind, latency: last.LatencyMS}
		if last.Online {
			s.up = 1
		}
		samples = append(samples, s)
	}
	metric("dedie_connection_up", "Whether the database answered its last check (1) or not (0).", "gauge")
	for _, s := range samples {
		fmt.Fprintf(w, "dedie_connection_up{connection=\"%s\",kind=\"%s\"} %d\n", label(s.name), s.kind, s.up)
	}
	metric("dedie_connection_latency_milliseconds", "Time the last check took to connect and query.", "gauge")
	for _, s := range samples {
		fmt.Fprintf(w, "dedie_connection_latency_milliseconds{connection=\"%s\",kind=\"%s\"} %d\n", label(s.name), s.kind, s.latency)
	}

	jobs, err := a.Store.ListJobs(ctx, actor.User.OrgID)
	if err != nil {
		return err
	}
	byStatus := map[string]int{}
	for _, j := range jobs {
		byStatus[j.Status]++
	}
	metric("dedie_migrations", "Number of migrations by status.", "gauge")
	for _, status := range []string{StatusDraft, StatusQueued, StatusRunning, StatusPaused, StatusFailed, StatusCompleted, StatusAborted} {
		fmt.Fprintf(w, "dedie_migrations{status=\"%s\"} %d\n", status, byStatus[status])
	}
	metric("dedie_migration_lag_bytes", "Source changes not yet applied to the target, for running migrations.", "gauge")
	for _, j := range jobs {
		if j.Status == StatusRunning {
			var p progress
			_ = json.Unmarshal(j.State, &p)
			fmt.Fprintf(w, "dedie_migration_lag_bytes{migration=\"%s\",id=\"%s\"} %d\n", label(j.Name), j.ID, p.LagBytes)
		}
	}
	metric("dedie_migration_rows_applied_total", "Row changes replayed on the target since the migration started.", "counter")
	for _, j := range jobs {
		if j.Status == StatusRunning {
			var p progress
			_ = json.Unmarshal(j.State, &p)
			fmt.Fprintf(w, "dedie_migration_rows_applied_total{migration=\"%s\",id=\"%s\"} %d\n", label(j.Name), j.ID, p.RowsApplied)
		}
	}

	backups, err := a.Store.ListBackups(ctx, actor.User.OrgID)
	if err != nil {
		return err
	}
	lastOK := map[string]time.Time{}
	failed := 0
	var bytes int64
	for _, b := range backups {
		switch b.Status {
		case "completed":
			bytes += b.SizeBytes
			if b.FinishedAt.After(lastOK[b.ConnectionName]) {
				lastOK[b.ConnectionName] = b.FinishedAt
			}
		case "failed":
			failed++
		}
	}
	metric("dedie_backup_last_success_timestamp_seconds", "When the newest completed backup of a connection finished.", "gauge")
	for name, t := range lastOK {
		fmt.Fprintf(w, "dedie_backup_last_success_timestamp_seconds{connection=\"%s\"} %d\n", label(name), t.Unix())
	}
	metric("dedie_backups_failed", "Failed backups currently on record.", "gauge")
	fmt.Fprintf(w, "dedie_backups_failed %d\n", failed)
	metric("dedie_backup_storage_bytes", "Disk space used by completed backups.", "gauge")
	fmt.Fprintf(w, "dedie_backup_storage_bytes %d\n", bytes)
	return nil
}

// Healthy reports whether the engine can reach its own metadata database.
func (a *App) Healthy(ctx context.Context) error {
	_, err := a.Store.CountOrgs(ctx)
	return err
}
