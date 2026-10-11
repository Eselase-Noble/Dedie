package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"dedie/engine/migrate"
	"dedie/engine/store"
)

const (
	leaseHeartbeat = 5 * time.Second
	// leaseStale is how long a job may go without a heartbeat before another
	// process takes it over.
	leaseStale = 30 * time.Second
)

// RunWorker executes queued migration jobs until ctx is cancelled, then waits
// for running jobs to stop cleanly. Jobs are leased, so several processes
// sharing one data directory never run the same job twice.
func (a *App) RunWorker(ctx context.Context) {
	var wg sync.WaitGroup
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		ids, err := a.Store.ClaimJobs(ctx, a.instance, leaseStale)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker: claim jobs: %v", err)
		}
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				a.runJob(ctx, id)
			}(id)
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
		}
	}
}

// RunJobForeground leases one job and runs it in the calling process; used by
// `dedie migrate run`.
func (a *App) RunJobForeground(ctx context.Context, id string) error {
	ok, err := a.Store.ClaimJob(ctx, id, a.instance, leaseStale)
	if err != nil {
		return err
	}
	if !ok {
		return conflict("the job is not queued, or another process is running it")
	}
	a.runJob(ctx, id)
	return nil
}

// reporter persists what the pipeline reports.
type reporter struct {
	store *store.Store
	jobID string
}

func (r reporter) Event(level, msg string) {
	r.store.AddJobEvent(context.Background(), r.jobID, level, msg)
}

func (r reporter) SetPhase(phase string) {
	r.store.SetJob(context.Background(), r.jobID, map[string]any{"phase": phase})
}

func (r reporter) SaveState(st any) {
	b, _ := json.Marshal(st)
	r.store.SetJob(context.Background(), r.jobID, map[string]any{"state": string(b)})
}

func (r reporter) ResetTables(tables []migrate.TableProgress) {
	rows := make([]store.JobTable, len(tables))
	for i, t := range tables {
		rows[i] = store.JobTable{Schema: t.Schema, Table: t.Table, EstRows: t.EstRows}
	}
	r.store.ResetJobTables(context.Background(), r.jobID, rows)
}

func (r reporter) UpdateTable(schema, table string, fields map[string]any) {
	r.store.SetJobTable(context.Background(), r.jobID, schema, table, fields)
}

func (r reporter) Desired() string {
	d, _ := r.store.JobDesired(context.Background(), r.jobID)
	return d
}

func (r reporter) SetDesired(d string) {
	r.store.SetJob(context.Background(), r.jobID, map[string]any{"desired": d})
}

func (a *App) runJob(ctx context.Context, id string) {
	bg := context.Background()
	rep := reporter{store: a.Store, jobID: id}
	j, err := a.Store.JobByID(ctx, id)
	if err != nil {
		return
	}
	fail := func(err error) {
		a.Store.SetJob(bg, id, map[string]any{"status": StatusFailed, "error": err.Error()})
		rep.Event("error", err.Error())
		a.emit(j.OrgID, Event{Type: EventMigrationFailed, Subject: "Migration " + j.Name + " failed", Message: err.Error()})
	}
	src, dst, kind, err := a.endpoints(ctx, j.OrgID, j.SourceID, j.TargetID)
	if err != nil {
		fail(err)
		return
	}
	pipeline, err := migrate.Lookup(kind)
	if err != nil {
		fail(err)
		return
	}
	var cfg migrate.Config
	_ = json.Unmarshal(j.Config, &cfg)

	// Keep the lease alive; if it is lost, another process owns the job and
	// this one must stop touching it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lost atomic.Bool
	go func() {
		tick := time.NewTicker(leaseHeartbeat)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ok, err := a.Store.Heartbeat(bg, id, a.instance); err == nil && !ok {
					lost.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	outcome, err := pipeline.Run(ctx, id, cfg, j.State, src, dst, rep)
	switch {
	case lost.Load():
		log.Printf("worker: lost the lease on %s; stopping", id)
	case err != nil && ctx.Err() != nil:
		// Shutting down: hand the job back so the next process resumes it.
		a.Store.SetJob(bg, id, map[string]any{"status": StatusQueued, "lease_owner": ""})
		rep.Event("info", "worker stopped; the job will resume when a worker starts")
	case err != nil:
		fail(err)
	default:
		status := map[migrate.Outcome]string{migrate.Completed: StatusCompleted, migrate.Paused: StatusPaused, migrate.Aborted: StatusAborted}[outcome]
		a.Store.SetJob(bg, id, map[string]any{"status": status, "desired": "", "error": ""})
		a.Store.Audit(bg, j.OrgID, "system", "migration."+status, id, fmt.Sprintf(`{"name":%q}`, j.Name), "")
		if outcome == migrate.Completed {
			a.emit(j.OrgID, Event{Type: EventMigrationCompleted, Subject: "Migration " + j.Name + " completed",
				Message: "The target has every change from the source and passed validation. Applications can use it now."})
		}
	}
}
