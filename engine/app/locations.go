package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"dedie/engine/secure"
	"dedie/engine/storage"
	"dedie/engine/store"
)

// LocationInput describes a place to keep backups: a directory on the
// machine that runs Dedie, or a bucket in S3-compatible object storage.
type LocationInput struct {
	Name string `json:"name"`
	// Kind is "dir" (the default) or "s3".
	Kind string `json:"kind"`
	// Path is the directory of a "dir" location.
	Path string           `json:"path"`
	S3   storage.S3Config `json:"s3"`
}

func locationAAD(orgID, id string) []byte { return []byte("location/" + orgID + "/" + id) }

// ListLocations lists the places backups may be written to, besides the
// data directory.
func (a *App) ListLocations(ctx context.Context, actor Actor) ([]store.Location, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	return a.Store.ListLocations(ctx, actor.User.OrgID)
}

// DefaultLocation is where backups go when no location is chosen.
func (a *App) DefaultLocation() string { return filepath.Join(a.dataDir, "backups") }

// locationStore opens the storage behind a location id; empty is the data
// directory.
func (a *App) locationStore(ctx context.Context, orgID, id string) (storage.Store, store.Location, error) {
	if id == "" {
		return storage.Dir{Path: a.DefaultLocation()}, store.Location{Kind: storage.KindDir, Path: a.DefaultLocation()}, nil
	}
	l, err := a.Store.GetLocation(ctx, orgID, id)
	if err != nil {
		return nil, l, invalid("backup location not found")
	}
	if l.Kind != storage.KindS3 {
		return storage.Dir{Path: l.Path}, l, nil
	}
	raw, err := secure.Open(a.keyring, l.ConfigEnc, locationAAD(l.OrgID, l.ID))
	if err != nil {
		return nil, l, err
	}
	var cfg storage.S3Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, l, err
	}
	s3, err := storage.NewS3(cfg)
	return s3, l, err
}

// backupStore opens the storage a backup's file is in.
func (a *App) backupStore(ctx context.Context, b store.Backup) (storage.Store, error) {
	// A backup written to a directory is found by its path even if the
	// location has since been removed.
	if !strings.HasPrefix(b.Path, "s3://") {
		return storage.Dir{}, nil
	}
	s, _, err := a.locationStore(ctx, b.OrgID, b.LocationID)
	if err != nil {
		return nil, conflict("the storage location of this backup no longer exists")
	}
	return s, nil
}

// CreateLocation registers a place to write backups. Only administrators
// may: it decides where Dedie sends an organization's data. The location
// is proven writable before it is saved, and object storage credentials
// are stored sealed.
func (a *App) CreateLocation(ctx context.Context, actor Actor, in LocationInput) (store.Location, error) {
	if err := actor.require(PermManageUsers); err != nil {
		return store.Location{}, err
	}
	in.Name, in.Path = strings.TrimSpace(in.Name), strings.TrimSpace(in.Path)
	if in.Name == "" || len(in.Name) > 80 {
		return store.Location{}, invalid("give the location a name of up to 80 characters")
	}
	l := store.Location{ID: secure.NewID("loc"), OrgID: actor.User.OrgID, Name: in.Name, Kind: storage.KindDir, CreatedBy: actor.User.Email}
	var target storage.Store
	switch in.Kind {
	case "", storage.KindDir:
		if !filepath.IsAbs(in.Path) {
			return l, invalid("enter the full path of a directory on the machine that runs Dedie, for example /mnt/backups/dedie")
		}
		l.Path = filepath.Clean(in.Path)
		if l.Path == string(filepath.Separator) {
			return l, invalid("choose a directory, not the root of the file system")
		}
		target = storage.Dir{Path: l.Path}
	case storage.KindS3:
		if err := in.S3.Validate(); err != nil {
			return l, invalid("%s", err.Error())
		}
		s3, err := storage.NewS3(in.S3)
		if err != nil {
			return l, invalid("%s", err.Error())
		}
		raw, _ := json.Marshal(in.S3)
		sealed, err := secure.Seal(a.keyring, raw, locationAAD(l.OrgID, l.ID))
		if err != nil {
			return l, err
		}
		l.Kind, l.Path, l.ConfigEnc, target = storage.KindS3, in.S3.Summary(), sealed, s3
	default:
		return l, invalid("unknown kind of location %q", in.Kind)
	}
	// Writing a file is the only reliable test that backups can be.
	if err := target.Check(ctx); err != nil {
		return l, invalid("Dedie cannot write to %s: %s", l.Path, err.Error())
	}
	if err := a.Store.SaveLocation(ctx, l); err != nil {
		return l, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, "backup.location_create", l.ID, map[string]string{"name": l.Name, "kind": l.Kind, "path": l.Path})
	return a.Store.GetLocation(ctx, l.OrgID, l.ID)
}

// DeleteLocation forgets a location. What is stored there is left alone,
// and a location that still holds backups cannot be removed.
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
