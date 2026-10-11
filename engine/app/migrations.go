package app

import (
	"context"
	"encoding/json"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/secure"
	"dedie/engine/store"
)

// Job statuses.
const (
	StatusDraft     = "draft"
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusFailed    = "failed"
	StatusCompleted = "completed"
	StatusAborted   = "aborted"
)

type MigrationInput struct {
	Name     string         `json:"name"`
	SourceID string         `json:"source_id"`
	TargetID string         `json:"target_id"`
	Config   migrate.Config `json:"config"`
}

// MigrationDetail is a job with its per-table progress and recent events.
type MigrationDetail struct {
	store.Job
	Tables []store.JobTable `json:"tables"`
	Events []store.JobEvent `json:"events"`
}

// endpoints resolves a job's two connections into driver configurations and
// the kind of pipeline that migrates between them.
func (a *App) endpoints(ctx context.Context, orgID, sourceID, targetID string) (src, dst connector.Config, kind string, err error) {
	sc, err := a.Store.GetConnection(ctx, orgID, sourceID)
	if err != nil {
		return src, dst, "", invalid("source connection not found")
	}
	tc, err := a.Store.GetConnection(ctx, orgID, targetID)
	if err != nil {
		return src, dst, "", invalid("target connection not found")
	}
	kind = migrate.Kind(sc.Kind, tc.Kind)
	if _, err := migrate.Lookup(kind); err != nil {
		return src, dst, kind, invalid("%s", err.Error())
	}
	if sc.ID == tc.ID || (sc.Host == tc.Host && sc.Port == tc.Port && sc.Database == tc.Database) {
		return src, dst, kind, invalid("source and target must be different databases")
	}
	if src, err = a.config(sc); err != nil {
		return src, dst, kind, err
	}
	dst, err = a.config(tc)
	return src, dst, kind, err
}

func (a *App) CreateMigration(ctx context.Context, actor Actor, in MigrationInput) (store.Job, error) {
	if err := actor.require(PermManageMigrations); err != nil {
		return store.Job{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return store.Job{}, invalid("name is required")
	}
	switch in.Config.Validation {
	case "", "checksum", "count", "none":
	default:
		return store.Job{}, invalid("validation must be checksum, count or none")
	}
	switch in.Config.Mode {
	case "", "migrate":
		in.Config.Mode = "migrate"
	case "sync":
		// A sync leaves the source in service by definition.
		in.Config.FenceSource = false
	default:
		return store.Job{}, invalid("mode must be migrate or sync")
	}
	_, _, kind, err := a.endpoints(ctx, actor.User.OrgID, in.SourceID, in.TargetID)
	if err != nil {
		return store.Job{}, err
	}
	// Store ids, not names, so renaming a connection does not break the job.
	sc, err := a.conn(ctx, actor, in.SourceID)
	if err != nil {
		return store.Job{}, invalid("source connection not found")
	}
	tc, err := a.conn(ctx, actor, in.TargetID)
	if err != nil {
		return store.Job{}, invalid("target connection not found")
	}
	cfg, _ := json.Marshal(in.Config)
	j := store.Job{ID: secure.NewID("mig"), OrgID: actor.User.OrgID, Name: strings.TrimSpace(in.Name), Kind: kind,
		SourceID: sc.ID, TargetID: tc.ID, Config: cfg, CreatedBy: actor.User.Email}
	if err := a.Store.CreateJob(ctx, j); err != nil {
		return j, err
	}
	a.audit(ctx, actor, "migration.create", j.ID, map[string]any{"name": j.Name, "source": sc.Name, "target": tc.Name, "config": in.Config})
	return a.Store.GetJob(ctx, actor.User.OrgID, j.ID)
}

func (a *App) ListMigrations(ctx context.Context, actor Actor) ([]store.Job, error) {
	if err := actor.require(PermViewMigrations); err != nil {
		return nil, err
	}
	return a.Store.ListJobs(ctx, actor.User.OrgID)
}

func (a *App) GetMigration(ctx context.Context, actor Actor, id string) (MigrationDetail, error) {
	var d MigrationDetail
	if err := actor.require(PermViewMigrations); err != nil {
		return d, err
	}
	j, err := a.Store.GetJob(ctx, actor.User.OrgID, id)
	if err != nil {
		return d, wrap(err)
	}
	d.Job = j
	if d.Tables, err = a.Store.ListJobTables(ctx, j.ID); err != nil {
		return d, err
	}
	d.Events, err = a.Store.ListJobEvents(ctx, j.ID, 200)
	return d, err
}

// PreflightMigration checks both databases without changing them and stores
// the report on the job.
func (a *App) PreflightMigration(ctx context.Context, actor Actor, id string) (*migrate.Report, error) {
	if err := actor.require(PermManageMigrations); err != nil {
		return nil, err
	}
	j, err := a.Store.GetJob(ctx, actor.User.OrgID, id)
	if err != nil {
		return nil, wrap(err)
	}
	src, dst, kind, err := a.endpoints(ctx, j.OrgID, j.SourceID, j.TargetID)
	if err != nil {
		return nil, err
	}
	pipeline, err := migrate.Lookup(kind)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	var cfg migrate.Config
	_ = json.Unmarshal(j.Config, &cfg)
	report, err := pipeline.Preflight(ctx, src, dst, cfg)
	if err != nil {
		return nil, &Error{Status: 502, Message: err.Error()}
	}
	b, _ := json.Marshal(report)
	if err := a.Store.SetJob(ctx, j.ID, map[string]any{"preflight": string(b)}); err != nil {
		return nil, err
	}
	a.audit(ctx, actor, "migration.preflight", j.ID, map[string]any{"ok": report.OK})
	return report, nil
}

// MigrationAction applies an operator instruction: start, pause, resume,
// cutover, abort or delete. The worker that owns the job picks the change up
// within about a second.
func (a *App) MigrationAction(ctx context.Context, actor Actor, id, action string) error {
	if err := actor.require(PermManageMigrations); err != nil {
		return err
	}
	j, err := a.Store.GetJob(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	set := map[string]any{}
	switch action {
	case "start", "resume":
		switch j.Status {
		case StatusDraft, StatusPaused, StatusFailed:
			set = map[string]any{"status": StatusQueued, "desired": "run", "error": ""}
		default:
			return conflict("a %s migration cannot be started", j.Status)
		}
	case "pause":
		if j.Status != StatusRunning {
			return conflict("only a running migration can be paused")
		}
		set["desired"] = "pause"
	case "cutover":
		if j.Status != StatusRunning || j.Phase != migrate.PhaseStreaming {
			return conflict("cutover is possible only while the migration is streaming changes")
		}
		set["desired"] = "cutover"
	case "abort":
		switch j.Status {
		case StatusCompleted, StatusAborted:
			return conflict("the migration is already %s", j.Status)
		case StatusDraft:
			set["status"] = StatusAborted
		case StatusRunning:
			set["desired"] = "abort"
		default:
			// Not running anywhere: queue it so a worker removes the
			// replication slot and publication from the source.
			set = map[string]any{"status": StatusQueued, "desired": "abort"}
		}
	case "delete":
		switch j.Status {
		case StatusDraft, StatusCompleted, StatusAborted:
		default:
			return conflict("abort the migration before deleting it")
		}
		if err := a.Store.DeleteJob(ctx, j.OrgID, j.ID); err != nil {
			return err
		}
		a.audit(ctx, actor, "migration.delete", j.ID, map[string]string{"name": j.Name})
		return nil
	default:
		return invalid("unknown action %q", action)
	}
	if err := a.Store.SetJob(ctx, j.ID, set); err != nil {
		return err
	}
	a.Store.AddJobEvent(ctx, j.ID, "info", action+" requested by "+actor.User.Email)
	a.audit(ctx, actor, "migration."+action, j.ID, map[string]string{"name": j.Name})
	return nil
}
