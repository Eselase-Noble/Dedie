package main

import (
	"context"
	"io"

	"dedie/engine/app"
	"dedie/engine/backup"
	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/store"
)

// backend is what the commands need from Dedie. There are two: local works
// on the data directory of this machine, remote talks to a Dedie server
// over its HTTP API. Commands are written once against this interface.
type backend interface {
	CreateUser(name, email, password, role string) (store.User, error)

	ListConnections() ([]store.Connection, error)
	TestInput(in app.ConnectionInput) (connector.ServerInfo, error)
	SaveConnection(id string, in app.ConnectionInput) (store.Connection, error)
	TestConnection(name string) (connector.ServerInfo, error)
	DeleteConnection(name string) error
	Query(conn string, req app.QueryRequest) (app.QueryResponse, error)

	CreateMigration(in app.MigrationInput) (store.Job, error)
	ListMigrations() ([]store.Job, error)
	GetMigration(id string) (app.MigrationDetail, error)
	PreflightMigration(id string) (*migrate.Report, error)
	MigrationAction(id, action string) error
	// RunJob returns when the job stops running: completed, paused or
	// failed. Locally the job executes in this process; remotely the
	// server executes it and this waits.
	RunJob(id string) error

	// CreateBackup and RestoreBackup return when the work has finished.
	CreateBackup(in app.BackupInput, wait bool) (store.Backup, error)
	ListBackups() ([]store.Backup, error)
	VerifyBackup(id string) error
	DeleteBackup(id string) error
	RestoreBackup(id string, in app.RestoreInput, wait bool) (store.Restore, error)
	ExportBackup(id string) (io.ReadCloser, string, error)
	ExtractBackup(id, dir string) (int, error)

	// ListLocations returns the default backup directory and the
	// locations added besides it.
	ListLocations() (string, []store.Location, error)
	CreateLocation(in app.LocationInput) (store.Location, error)
	DeleteLocation(name string) error

	SaveSchedule(id string, in app.ScheduleInput) (store.Schedule, error)
	ListSchedules() ([]store.Schedule, error)
	DeleteSchedule(id string) error

	ListAudit(before int64, limit int) ([]store.AuditEntry, error)
	VerifyAudit() (app.AuditVerification, error)
}

// local runs commands directly on this machine's data directory. Whoever
// can read that directory and its master key is trusted as an owner.
type local struct {
	ctx   context.Context
	app   *app.App
	actor app.Actor
}

func (l *local) CreateUser(name, email, password, role string) (store.User, error) {
	return l.app.CreateUser(l.ctx, l.actor, name, email, password, role)
}
func (l *local) ListConnections() ([]store.Connection, error) {
	return l.app.ListConnections(l.ctx, l.actor)
}
func (l *local) TestInput(in app.ConnectionInput) (connector.ServerInfo, error) {
	return l.app.TestInput(l.ctx, l.actor, in)
}
func (l *local) SaveConnection(id string, in app.ConnectionInput) (store.Connection, error) {
	return l.app.SaveConnection(l.ctx, l.actor, id, in)
}
func (l *local) TestConnection(name string) (connector.ServerInfo, error) {
	return l.app.TestConnection(l.ctx, l.actor, name)
}
func (l *local) DeleteConnection(name string) error {
	return l.app.DeleteConnection(l.ctx, l.actor, name)
}
func (l *local) Query(conn string, req app.QueryRequest) (app.QueryResponse, error) {
	return l.app.Query(l.ctx, l.actor, conn, req)
}
func (l *local) CreateMigration(in app.MigrationInput) (store.Job, error) {
	return l.app.CreateMigration(l.ctx, l.actor, in)
}
func (l *local) ListMigrations() ([]store.Job, error) { return l.app.ListMigrations(l.ctx, l.actor) }
func (l *local) GetMigration(id string) (app.MigrationDetail, error) {
	return l.app.GetMigration(l.ctx, l.actor, id)
}
func (l *local) PreflightMigration(id string) (*migrate.Report, error) {
	return l.app.PreflightMigration(l.ctx, l.actor, id)
}
func (l *local) MigrationAction(id, action string) error {
	return l.app.MigrationAction(l.ctx, l.actor, id, action)
}
func (l *local) RunJob(id string) error { return l.app.RunJobForeground(l.ctx, id) }
func (l *local) CreateBackup(in app.BackupInput, wait bool) (store.Backup, error) {
	return l.app.CreateBackup(l.ctx, l.actor, in, wait)
}
func (l *local) ListBackups() ([]store.Backup, error) { return l.app.ListBackups(l.ctx, l.actor) }
func (l *local) VerifyBackup(id string) error         { return l.app.VerifyBackup(l.ctx, l.actor, id) }
func (l *local) DeleteBackup(id string) error         { return l.app.DeleteBackup(l.ctx, l.actor, id) }
func (l *local) RestoreBackup(id string, in app.RestoreInput, wait bool) (store.Restore, error) {
	return l.app.RestoreBackup(l.ctx, l.actor, id, in, wait)
}
func (l *local) ExportBackup(id string) (io.ReadCloser, string, error) {
	return l.app.ExportBackup(l.ctx, l.actor, id)
}
func (l *local) ExtractBackup(id, dir string) (int, error) {
	return l.app.ExtractBackup(l.ctx, l.actor, id, dir)
}
func (l *local) ListLocations() (string, []store.Location, error) {
	list, err := l.app.ListLocations(l.ctx, l.actor)
	return l.app.DefaultLocation(), list, err
}
func (l *local) CreateLocation(in app.LocationInput) (store.Location, error) {
	return l.app.CreateLocation(l.ctx, l.actor, in)
}
func (l *local) DeleteLocation(name string) error { return l.app.DeleteLocation(l.ctx, l.actor, name) }
func (l *local) SaveSchedule(id string, in app.ScheduleInput) (store.Schedule, error) {
	return l.app.SaveSchedule(l.ctx, l.actor, id, in)
}
func (l *local) ListSchedules() ([]store.Schedule, error) { return l.app.ListSchedules(l.ctx, l.actor) }
func (l *local) DeleteSchedule(id string) error           { return l.app.DeleteSchedule(l.ctx, l.actor, id) }
func (l *local) ListAudit(before int64, limit int) ([]store.AuditEntry, error) {
	return l.app.ListAudit(l.ctx, l.actor, before, limit)
}
func (l *local) VerifyAudit() (app.AuditVerification, error) {
	return l.app.VerifyAudit(l.ctx, l.actor)
}

// extractDownloaded unpacks a physical backup that was fetched from a
// server into a directory on this machine.
func extractDownloaded(src io.ReadCloser, dir string) (int, error) {
	defer src.Close()
	return backup.ExtractPhysical(src, dir)
}
