package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"dedie/engine/secure"
	"dedie/engine/store"
)

// ListLocations lists the directories backups may be written to, besides
// the data directory.
func (a *App) ListLocations(ctx context.Context, actor Actor) ([]store.Location, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	return a.Store.ListLocations(ctx, actor.User.OrgID)
}

// DefaultLocation is where backups go when no location is chosen.
func (a *App) DefaultLocation() string { return filepath.Join(a.dataDir, "backups") }

// CreateLocation registers a directory on this machine as a place to
// write backups. Only administrators may: it decides where on the host
// Dedie writes files. The directory is created if needed and must be
// writable by the account Dedie runs as.
func (a *App) CreateLocation(ctx context.Context, actor Actor, name, path string) (store.Location, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return store.Location{}, err
	}
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if name == "" || len(name) > 80 {
		return store.Location{}, invalid("give the location a name of up to 80 characters")
	}
	if !filepath.IsAbs(path) {
		return store.Location{}, invalid("enter the full path of a directory on the machine that runs Dedie, for example /mnt/backups/dedie")
	}
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return store.Location{}, invalid("choose a directory, not the root of the file system")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return store.Location{}, invalid("the directory cannot be created: %s", err.Error())
	}
	// Writing a file is the only reliable test that backups can be.
	probe, err := os.CreateTemp(path, ".dedie-write-test-*")
	if err != nil {
		return store.Location{}, invalid("Dedie cannot write to %s: %s", path, err.Error())
	}
	probe.Close()
	os.Remove(probe.Name())

	l := store.Location{ID: secure.NewID("loc"), OrgID: actor.User.OrgID, Name: name, Path: path, CreatedBy: actor.User.Email}
	if err := a.Store.SaveLocation(ctx, l); err != nil {
		return l, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, "backup.location_create", l.ID, map[string]string{"name": name, "path": path})
	return a.Store.GetLocation(ctx, l.OrgID, l.ID)
}

// DeleteLocation forgets a location. Its directory and files are left
// alone, and a location that still holds backups cannot be removed.
func (a *App) DeleteLocation(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermManageUsers); err != nil {
		return err
	}
	l, err := a.Store.GetLocation(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if l.Backups > 0 {
		return conflict("%d backup(s) are stored in this location; delete them first", l.Backups)
	}
	if err := a.Store.DeleteLocation(ctx, l.OrgID, l.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "backup.location_delete", l.ID, map[string]string{"name": l.Name, "path": l.Path})
	return nil
}
