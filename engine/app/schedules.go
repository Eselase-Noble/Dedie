package app

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"dedie/engine/backup"
	"dedie/engine/schedule"
	"dedie/engine/secure"
	"dedie/engine/store"
)

// ScheduleInput is what a user submits to create or change a backup schedule.
type ScheduleInput struct {
	Name         string   `json:"name"`
	ConnectionID string   `json:"connection_id"`
	Type         string   `json:"type"`
	Tables       []string `json:"tables"`
	Level        string   `json:"level"`
	LocationID   string   `json:"location_id"`
	Plain        bool     `json:"plain"`
	Cron         string   `json:"cron"`
	KeepLast     int      `json:"keep_last"`
	KeepDays     int      `json:"keep_days"`
	Verify       bool     `json:"verify"`
	Enabled      bool     `json:"enabled"`
}

func (a *App) ListSchedules(ctx context.Context, actor Actor) ([]store.Schedule, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	return a.Store.ListSchedules(ctx, actor.User.OrgID)
}

// SaveSchedule creates a schedule, or updates the one named by id.
func (a *App) SaveSchedule(ctx context.Context, actor Actor, id string, in ScheduleInput) (store.Schedule, error) {
	// A schedule deletes old backups under its retention rule, so it needs
	// the permission that deleting a backup by hand does.
	if err := actor.require(PermRestore); err != nil {
		return store.Schedule{}, err
	}
	in.Name, in.Cron = strings.TrimSpace(in.Name), strings.TrimSpace(in.Cron)
	if in.Name == "" {
		return store.Schedule{}, invalid("give the schedule a name")
	}
	if in.Type == "" {
		in.Type = backup.TypeFull
	}
	spec, err := schedule.Parse(in.Cron)
	if err != nil {
		return store.Schedule{}, invalid("%s", err.Error())
	}
	next := spec.Next(time.Now())
	if next.IsZero() {
		return store.Schedule{}, invalid("that schedule never runs")
	}
	if in.KeepLast < 0 || in.KeepDays < 0 {
		return store.Schedule{}, invalid("retention values cannot be negative")
	}
	c, err := a.conn(ctx, actor, in.ConnectionID)
	if err != nil {
		return store.Schedule{}, invalid("connection not found")
	}
	opts := backup.Options{Type: in.Type, Tables: in.Tables}
	if in.Type != backup.TypeTables {
		opts.Tables = nil
	}
	if err := backup.CheckOptions(c.Kind, opts); err != nil {
		return store.Schedule{}, invalid("%s", err.Error())
	}
	switch in.Level {
	case "":
		in.Level = backup.LevelFull
	case backup.LevelFull:
	case backup.LevelDifferential, backup.LevelIncremental:
		if !backup.LevelsSupported(c.Kind, in.Type) {
			return store.Schedule{}, invalid("%s backups are available for physical PostgreSQL backups and full logical MySQL backups", in.Level)
		}
	default:
		return store.Schedule{}, invalid("level must be full, differential or incremental")
	}
	if in.LocationID != "" {
		loc, err := a.Store.GetLocation(ctx, actor.User.OrgID, in.LocationID)
		if err != nil {
			return store.Schedule{}, invalid("backup location not found")
		}
		in.LocationID = loc.ID
	}
	sc := store.Schedule{ID: secure.NewID("sch"), OrgID: actor.User.OrgID, CreatedBy: actor.User.Email}
	action := "schedule.create"
	if id != "" {
		if sc, err = a.Store.GetSchedule(ctx, actor.User.OrgID, id); err != nil {
			return sc, wrap(err)
		}
		action = "schedule.update"
	}
	sc.Name, sc.ConnectionID, sc.Type, sc.Tables, sc.Cron = in.Name, c.ID, in.Type, opts.Tables, in.Cron
	sc.KeepLast, sc.KeepDays, sc.Verify, sc.Enabled, sc.NextRunAt = in.KeepLast, in.KeepDays, in.Verify, in.Enabled, next
	sc.Level, sc.LocationID, sc.Encrypted = in.Level, in.LocationID, !in.Plain
	if err := a.Store.SaveSchedule(ctx, sc); err != nil {
		return sc, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, action, sc.ID, map[string]any{"name": sc.Name, "connection": c.Name, "cron": sc.Cron, "type": sc.Type, "level": sc.Level,
		"keep_last": sc.KeepLast, "keep_days": sc.KeepDays, "enabled": sc.Enabled})
	return sc, nil
}

// DeleteSchedule removes a schedule. The backups it took are kept.
func (a *App) DeleteSchedule(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermRestore); err != nil {
		return err
	}
	sc, err := a.Store.GetSchedule(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if err := a.Store.DeleteSchedule(ctx, sc.OrgID, sc.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "schedule.delete", sc.ID, map[string]string{"name": sc.Name})
	return nil
}

// RunScheduleNow takes the schedule's backup immediately, without moving
// its next run time.
func (a *App) RunScheduleNow(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermBackup); err != nil {
		return err
	}
	sc, err := a.Store.GetSchedule(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	a.audit(ctx, actor, "schedule.run", sc.ID, map[string]string{"name": sc.Name})
	go a.runSchedule(context.Background(), sc)
	return nil
}

// RunScheduler takes scheduled backups until ctx is cancelled. Claiming a
// schedule is atomic, so several processes on one data directory run each
// backup once.
func (a *App) RunScheduler(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		due, err := a.Store.DueSchedules(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			log.Printf("scheduler: %v", err)
		}
		for _, sc := range due {
			spec, err := schedule.Parse(sc.Cron)
			if err != nil {
				a.Store.FinishSchedule(ctx, sc.ID, "failed", err.Error())
				continue
			}
			// The next run is counted from now, not from the missed time:
			// after downtime one backup is taken, not one per missed slot.
			if ok, err := a.Store.ClaimSchedule(ctx, sc.ID, sc.NextRunAt, spec.Next(time.Now())); err != nil || !ok {
				continue
			}
			go a.runSchedule(context.Background(), sc)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// runSchedule takes one backup for a schedule, verifies it when asked and
// then applies the retention rule.
func (a *App) runSchedule(ctx context.Context, sc store.Schedule) {
	actor := Actor{User: store.User{ID: "system", OrgID: sc.OrgID, Email: "schedule:" + sc.Name, Role: RoleOwner}, IP: "local"}
	finish := func(err error) {
		if err != nil {
			a.Store.FinishSchedule(ctx, sc.ID, "failed", err.Error())
			a.emit(sc.OrgID, Event{Type: EventBackupFailed, Subject: sc.Name, Message: "Scheduled backup " + sc.Name + " failed: " + err.Error()})
			return
		}
		a.Store.FinishSchedule(ctx, sc.ID, "completed", "")
	}
	b, err := a.startBackup(ctx, actor, BackupInput{ConnectionID: sc.ConnectionID, Type: sc.Type, Tables: sc.Tables, Level: sc.Level,
		LocationID: sc.LocationID, Plain: !sc.Encrypted, Note: "Scheduled: " + sc.Name}, sc.ID, true)
	if err != nil {
		finish(err)
		return
	}
	if b.Status != "completed" {
		finish(&Error{Status: 500, Message: b.Error})
		return
	}
	if sc.Verify {
		if err := a.verifyBackupFile(b); err != nil {
			finish(&Error{Status: 500, Message: "the backup was written but failed verification: " + err.Error()})
			return
		}
		a.Store.MarkBackupVerified(ctx, b.ID)
	}
	removed, err := a.applyRetention(ctx, actor, sc)
	if removed > 0 {
		log.Printf("scheduler: %s: removed %d expired backup(s)", sc.Name, removed)
	}
	finish(err)
}

// applyRetention deletes the schedule's backups that its rule no longer
// keeps. A backup is removed when it falls outside either limit; the newest
// one always stays, so a schedule never deletes its only good backup.
func (a *App) applyRetention(ctx context.Context, actor Actor, sc store.Schedule) (int, error) {
	if sc.KeepLast == 0 && sc.KeepDays == 0 {
		return 0, nil
	}
	list, err := a.Store.ScheduleBackups(ctx, sc.ID)
	if err != nil {
		return 0, err
	}
	removed := 0
	cutoff := time.Now().AddDate(0, 0, -sc.KeepDays)
	for i, b := range list {
		expired := sc.KeepLast > 0 && i >= sc.KeepLast || sc.KeepDays > 0 && b.CreatedAt.Before(cutoff)
		if i == 0 || !expired {
			continue
		}
		// A backup that later ones build on stays until they have gone;
		// deleting it would make them unrestorable.
		if n, err := a.Store.BackupDependents(ctx, b.ID); err != nil || n > 0 {
			continue
		}
		if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		if err := a.Store.DeleteBackup(ctx, b.OrgID, b.ID); err != nil {
			return removed, err
		}
		a.audit(ctx, actor, "backup.expire", b.ID, map[string]string{"schedule": sc.Name, "database": b.Database})
		removed++
	}
	return removed, nil
}
