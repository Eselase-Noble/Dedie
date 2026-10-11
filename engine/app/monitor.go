package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"dedie/engine/store"
)

const (
	monitorInterval = time.Minute
	// healthRetention is how long reachability checks are kept.
	healthRetention = 7 * 24 * time.Hour
	// failuresBeforeDown is how many checks in a row must fail before a
	// database is reported down, so one dropped packet raises no alarm.
	failuresBeforeDown = 2
	// lagAlertBytes is the unapplied change volume at which a migration
	// is reported as falling behind.
	lagAlertBytes = 512 << 20
)

// progress is the part of every pipeline's saved state that monitoring
// reads.
type progress struct {
	LagBytes       int64     `json:"lag_bytes"`
	RowsApplied    int64     `json:"rows_applied"`
	TxApplied      int64     `json:"tx_applied"`
	SnapshotDone   bool      `json:"snapshot_done"`
	LastCommitTime time.Time `json:"last_commit_time"`
}

// monitorState remembers what was last reported, so that an alert is
// raised when something changes rather than on every check.
type monitorState struct {
	mu       sync.Mutex
	failures map[string]int  // consecutive failed checks per connection
	down     map[string]bool // connections reported down
	lagging  map[string]bool // migrations reported as lagging
}

// RunMonitor checks every connection once a minute, records the result
// and raises alerts when a database goes down or comes back, or when a
// migration falls behind. It runs until ctx is cancelled.
func (a *App) RunMonitor(ctx context.Context) {
	st := &monitorState{failures: map[string]int{}, down: map[string]bool{}, lagging: map[string]bool{}}
	tick := time.NewTicker(monitorInterval)
	defer tick.Stop()
	for {
		orgs, err := a.Store.OrgIDs(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("monitor: %v", err)
		}
		for _, org := range orgs {
			a.checkConnections(ctx, st, org)
			a.checkLag(ctx, st, org)
		}
		a.Store.PruneHealth(ctx, time.Now().Add(-healthRetention))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) checkConnections(ctx context.Context, st *monitorState, orgID string) {
	conns, err := a.Store.ListConnections(ctx, orgID)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c store.Connection) {
			defer wg.Done()
			status := a.probe(ctx, c)
			if ctx.Err() != nil {
				return // shutting down: a cancelled check says nothing
			}
			a.Store.AddHealthSample(ctx, c.ID, status.Online, status.LatencyMS, status.Error)
			st.mu.Lock()
			defer st.mu.Unlock()
			switch {
			case status.Online && st.down[c.ID]:
				st.down[c.ID] = false
				a.emit(orgID, Event{Type: EventConnectionUp, Subject: c.Name + " is back", Message: fmt.Sprintf("%s (%s:%d/%s) answers again.", c.Name, c.Host, c.Port, c.Database)})
			case status.Online:
				st.failures[c.ID] = 0
			default:
				st.failures[c.ID]++
				if st.failures[c.ID] == failuresBeforeDown && !st.down[c.ID] {
					st.down[c.ID] = true
					a.emit(orgID, Event{Type: EventConnectionDown, Subject: c.Name + " is not answering",
						Message: fmt.Sprintf("%s (%s:%d/%s) failed %d checks in a row: %s", c.Name, c.Host, c.Port, c.Database, failuresBeforeDown, status.Error)})
				}
			}
			if status.Online {
				st.failures[c.ID] = 0
			}
		}(c)
	}
	wg.Wait()
}

func (a *App) checkLag(ctx context.Context, st *monitorState, orgID string) {
	jobs, err := a.Store.ListJobs(ctx, orgID)
	if err != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, j := range jobs {
		var p progress
		_ = json.Unmarshal(j.State, &p)
		behind := j.Status == StatusRunning && p.SnapshotDone && p.LagBytes > lagAlertBytes
		if behind && !st.lagging[j.ID] {
			a.emit(orgID, Event{Type: EventMigrationLag, Subject: j.Name + " is falling behind",
				Message: fmt.Sprintf("Migration %s has %d MB of source changes waiting to be applied. The target may be overloaded, or the source is writing faster than changes can be replayed.", j.Name, p.LagBytes>>20)})
		}
		st.lagging[j.ID] = behind
	}
}

// ConnectionHealth is a connection's recent reachability.
type ConnectionHealth struct {
	ConnectionID string `json:"connection_id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	// Checks is the number of samples in the window; UptimePercent is
	// meaningful only when it is above zero.
	Checks        int                  `json:"checks"`
	UptimePercent float64              `json:"uptime_percent"`
	AvgLatencyMS  int64                `json:"avg_latency_ms"`
	Last          *store.HealthSample  `json:"last"`
	Samples       []store.HealthSample `json:"samples"`
}

// MigrationHealth is the live progress of a migration.
type MigrationHealth struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	Phase          string    `json:"phase"`
	LagBytes       int64     `json:"lag_bytes"`
	RowsApplied    int64     `json:"rows_applied"`
	LastCommitTime time.Time `json:"last_commit_time"`
}

// Monitoring is everything the monitoring screen shows.
type Monitoring struct {
	Connections []ConnectionHealth `json:"connections"`
	Migrations  []MigrationHealth  `json:"migrations"`
	Events      []store.AlertEvent `json:"events"`
}

// GetMonitoring summarizes the last day of checks for the connections the
// actor may see, the migrations in progress and recent alerts.
func (a *App) GetMonitoring(ctx context.Context, actor Actor) (Monitoring, error) {
	var out Monitoring
	if err := actor.require(PermServerAdmin); err != nil {
		return out, err
	}
	conns, err := a.visibleConnections(ctx, actor)
	if err != nil {
		return out, err
	}
	since := time.Now().Add(-24 * time.Hour)
	out.Connections = []ConnectionHealth{}
	for _, c := range conns {
		samples, err := a.Store.HealthSamples(ctx, c.ID, since)
		if err != nil {
			return out, err
		}
		h := ConnectionHealth{ConnectionID: c.ID, Name: c.Name, Kind: c.Kind, Checks: len(samples)}
		var up int
		var latency int64
		for _, s := range samples {
			if s.Online {
				up++
				latency += s.LatencyMS
			}
		}
		if len(samples) > 0 {
			h.UptimePercent = float64(up) * 100 / float64(len(samples))
			h.Last = &samples[len(samples)-1]
		}
		if up > 0 {
			h.AvgLatencyMS = latency / int64(up)
		}
		h.Samples = thin(samples, 120)
		out.Connections = append(out.Connections, h)
	}
	jobs, err := a.Store.ListJobs(ctx, actor.User.OrgID)
	if err != nil {
		return out, err
	}
	out.Migrations = []MigrationHealth{}
	for _, j := range jobs {
		if j.Status != StatusRunning && j.Status != StatusQueued && j.Status != StatusPaused && j.Status != StatusFailed {
			continue
		}
		var p progress
		_ = json.Unmarshal(j.State, &p)
		out.Migrations = append(out.Migrations, MigrationHealth{ID: j.ID, Name: j.Name, Status: j.Status, Phase: j.Phase,
			LagBytes: p.LagBytes, RowsApplied: p.RowsApplied, LastCommitTime: p.LastCommitTime})
	}
	out.Events, err = a.Store.ListAlertEvents(ctx, actor.User.OrgID, 50)
	return out, err
}

// thin reduces a series to at most n points for charting. Within each
// bucket an outage wins over a success, so thinning never hides one.
func thin(samples []store.HealthSample, n int) []store.HealthSample {
	if len(samples) <= n {
		return samples
	}
	out := make([]store.HealthSample, 0, n)
	for i := 0; i < n; i++ {
		lo, hi := i*len(samples)/n, (i+1)*len(samples)/n
		pick := samples[hi-1]
		for _, s := range samples[lo:hi] {
			if !s.Online {
				pick = s
				break
			}
		}
		out = append(out, pick)
	}
	return out
}
