package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dedie/engine/backup"
	"dedie/engine/secure"
	"dedie/engine/store"
)

func backupAAD(orgID, backupID string) []byte { return []byte("backup/" + orgID + "/" + backupID) }

func (a *App) ListBackups(ctx context.Context, actor Actor) ([]store.Backup, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	return a.Store.ListBackups(ctx, actor.User.OrgID)
}

func (a *App) ListRestores(ctx context.Context, actor Actor) ([]store.Restore, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	return a.Store.ListRestores(ctx, actor.User.OrgID)
}

// BackupInput describes a backup to take.
type BackupInput struct {
	ConnectionID string `json:"connection_id"`
	Note         string `json:"note"`
	// Type is full (the default), schema, data, tables or physical.
	Type   string   `json:"type"`
	Tables []string `json:"tables"`
}

// CreateBackup starts a backup of a connection's database and returns
// immediately; the backup's status moves from "running" to "completed" or
// "failed". With wait set the call blocks until it finishes.
func (a *App) CreateBackup(ctx context.Context, actor Actor, in BackupInput, wait bool) (store.Backup, error) {
	if err := actor.require(PermBackup); err != nil {
		return store.Backup{}, err
	}
	return a.startBackup(ctx, actor, in, "", wait)
}

// startBackup does the work of CreateBackup for a user or for a schedule.
func (a *App) startBackup(ctx context.Context, actor Actor, in BackupInput, scheduleID string, wait bool) (store.Backup, error) {
	if in.Type == "" {
		in.Type = backup.TypeFull
	}
	c, sess, err := a.open(ctx, actor.User.OrgID, in.ConnectionID)
	if err != nil {
		return store.Backup{}, err
	}
	info, err := sess.Info(ctx)
	sess.Close(context.WithoutCancel(ctx))
	if err != nil {
		return store.Backup{}, err
	}
	if !backup.Supported(c.Kind) {
		return store.Backup{}, invalid("backups are not supported for %s connections yet", c.Kind)
	}
	opts := backup.Options{Type: in.Type, Tables: in.Tables}
	if in.Type != backup.TypeTables {
		opts.Tables = nil
	}
	if err := backup.CheckOptions(c.Kind, opts); err != nil {
		return store.Backup{}, invalid("%s", err.Error())
	}
	cfg, err := a.config(c)
	if err != nil {
		return store.Backup{}, err
	}
	b := store.Backup{ID: secure.NewID("bak"), OrgID: c.OrgID, ConnectionID: c.ID, ConnectionName: c.Name, Database: c.Database,
		Kind: c.Kind, Type: opts.Type, Format: backup.Format(c.Kind, opts.Type), Tables: opts.Tables, ScheduleID: scheduleID,
		ServerVersion: info.Version, Status: "running", Note: in.Note, CreatedBy: actor.User.Email}
	b.Path = filepath.Join(a.dataDir, "backups", b.ID+".dediebak")
	if err := a.Store.CreateBackup(ctx, b); err != nil {
		return b, err
	}
	a.audit(ctx, actor, "backup.create", b.ID, map[string]any{"connection": c.Name, "database": c.Database, "type": b.Type, "tables": b.Tables})

	run := func() {
		bg := context.Background()
		size, sum, err := func() (int64, string, error) {
			f, err := os.OpenFile(b.Path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return 0, "", err
			}
			defer f.Close()
			// The digest covers the encrypted file, so integrity can be
			// checked later without the key.
			hash := sha256.New()
			enc, err := secure.NewStreamWriter(a.keyring, io.MultiWriter(f, hash), backupAAD(b.OrgID, b.ID))
			if err != nil {
				return 0, "", err
			}
			if err := backup.Dump(bg, c.Kind, cfg, info.VersionNum, opts, enc); err != nil {
				return 0, "", err
			}
			if err := enc.Close(); err != nil {
				return 0, "", err
			}
			if err := f.Sync(); err != nil {
				return 0, "", err
			}
			st, err := f.Stat()
			if err != nil {
				return 0, "", err
			}
			return st.Size(), hex.EncodeToString(hash.Sum(nil)), nil
		}()
		if err != nil {
			os.Remove(b.Path)
			a.Store.FinishBackup(bg, b.ID, "failed", err.Error(), 0, "")
			return
		}
		a.Store.FinishBackup(bg, b.ID, "completed", "", size, sum)
	}
	if wait {
		run()
	} else {
		go run()
	}
	out, err := a.Store.GetBackup(ctx, b.OrgID, b.ID)
	return out, err
}

func (a *App) DeleteBackup(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermRestore); err != nil {
		return err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if b.Status == "running" {
		return conflict("the backup is still running")
	}
	if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := a.Store.DeleteBackup(ctx, b.OrgID, b.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "backup.delete", b.ID, map[string]string{"connection": b.ConnectionName, "database": b.Database})
	return nil
}

// VerifyBackup checks a backup file against the digest recorded when it was
// written and confirms the whole archive still decrypts.
func (a *App) VerifyBackup(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermBackup); err != nil {
		return err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if err := a.verifyBackupFile(b); err != nil {
		return err
	}
	return a.Store.MarkBackupVerified(ctx, b.ID)
}

// openBackup returns the decrypted contents of a completed backup.
func (a *App) openBackup(b store.Backup) (io.ReadCloser, error) {
	if b.Status != "completed" {
		return nil, conflict("the backup is %s", b.Status)
	}
	f, err := os.Open(b.Path)
	if err != nil {
		return nil, conflict("the backup file is missing from the data directory")
	}
	dec, err := secure.NewStreamReader(a.keyring, f, backupAAD(b.OrgID, b.ID))
	if err != nil {
		f.Close()
		return nil, conflict("%s", err.Error())
	}
	return struct {
		io.Reader
		io.Closer
	}{dec, f}, nil
}

// ExportBackup verifies a backup and returns its decrypted archive, in the
// database's own format, with a file name to save it under. The copy leaves
// Dedie's protection, so it needs the restore permission and is audited.
func (a *App) ExportBackup(ctx context.Context, actor Actor, id string) (io.ReadCloser, string, error) {
	if err := actor.require(PermRestore); err != nil {
		return nil, "", err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, id)
	if err != nil {
		return nil, "", wrap(err)
	}
	if err := a.verifyBackupFile(b); err != nil {
		return nil, "", err
	}
	r, err := a.openBackup(b)
	if err != nil {
		return nil, "", err
	}
	name := safeFileName(b.Database) + "-" + b.CreatedAt.UTC().Format("20060102-150405") + backup.Extension(b.Format)
	if b.Format == "" {
		name = safeFileName(b.Database) + "-" + b.CreatedAt.UTC().Format("20060102-150405") + backup.Extension(backup.Format(b.Kind, ""))
		if b.Kind == "mysql" {
			name = strings.TrimSuffix(name, ".gz")
		}
	}
	a.audit(ctx, actor, "backup.export", b.ID, map[string]string{"connection": b.ConnectionName, "database": b.Database})
	return r, name, nil
}

func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

// ExtractBackup unpacks a physical backup into an empty directory on this
// machine, ready for PostgreSQL to be started on it.
func (a *App) ExtractBackup(ctx context.Context, actor Actor, id, dir string) (int, error) {
	if err := actor.require(PermRestore); err != nil {
		return 0, err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, id)
	if err != nil {
		return 0, wrap(err)
	}
	if b.Format != backup.FormatPGBase {
		return 0, invalid("only physical backups are extracted; restore this one into a connection")
	}
	if err := a.verifyBackupFile(b); err != nil {
		return 0, err
	}
	r, err := a.openBackup(b)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	files, err := backup.ExtractPhysical(r, dir)
	if err != nil {
		return files, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, "backup.extract", b.ID, map[string]string{"connection": b.ConnectionName, "directory": dir})
	return files, nil
}

func (a *App) verifyBackupFile(b store.Backup) error {
	if b.Status != "completed" {
		return conflict("the backup is %s", b.Status)
	}
	f, err := os.Open(b.Path)
	if err != nil {
		return conflict("the backup file is missing from the data directory")
	}
	defer f.Close()
	hash := sha256.New()
	dec, err := secure.NewStreamReader(a.keyring, io.TeeReader(f, hash), backupAAD(b.OrgID, b.ID))
	if err != nil {
		return conflict("%s", err.Error())
	}
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return conflict("%s", err.Error())
	}
	if hex.EncodeToString(hash.Sum(nil)) != b.SHA256 {
		return conflict("the backup file does not match its recorded checksum")
	}
	return nil
}

// RestoreInput describes a restore. Confirm must repeat the name of the
// database being written to: a restore with Clean replaces existing objects
// and cannot be undone.
type RestoreInput struct {
	TargetID string `json:"target_id"`
	// NewDatabase, when set, is created on the target's server and the
	// backup is restored into it, leaving the connection's own database
	// untouched.
	NewDatabase string `json:"new_database"`
	// Tables restores only these tables (PostgreSQL archives).
	Tables     []string `json:"tables"`
	Clean      bool     `json:"clean"`
	KeepOwners bool     `json:"keep_owners"`
	Confirm    string   `json:"confirm"`
}

// RestoreBackup verifies a backup and applies it to the target connection.
// It returns once the restore has started unless wait is set.
func (a *App) RestoreBackup(ctx context.Context, actor Actor, backupID string, in RestoreInput, wait bool) (store.Restore, error) {
	if err := actor.require(PermRestore); err != nil {
		return store.Restore{}, err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, backupID)
	if err != nil {
		return store.Restore{}, wrap(err)
	}
	if b.Format == backup.FormatPGBase {
		return store.Restore{}, invalid("a physical backup is a copy of the server's data files and is not restored into a running database; extract it with `dedie backup extract %s <directory>` and start PostgreSQL on that directory", b.ID)
	}
	target, err := a.Store.GetConnection(ctx, actor.User.OrgID, in.TargetID)
	if err != nil {
		return store.Restore{}, invalid("target connection not found")
	}
	if target.Kind != b.Kind {
		return store.Restore{}, invalid("a %s backup cannot be restored into a %s database", b.Kind, target.Kind)
	}
	if target.ReadOnly {
		return store.Restore{}, &Error{Status: 403, Message: "the target connection is marked read-only"}
	}
	in.NewDatabase = strings.TrimSpace(in.NewDatabase)
	database := target.Database
	if in.NewDatabase != "" {
		database = in.NewDatabase
	}
	if in.Confirm != database {
		return store.Restore{}, invalid("type the target database name (%s) to confirm the restore", database)
	}
	if len(in.Tables) > 0 && b.Format != backup.FormatPGCustom && !(b.Format == "" && b.Kind == "postgres") {
		return store.Restore{}, invalid("restoring selected tables is available for PostgreSQL backups only")
	}
	if err := a.verifyBackupFile(b); err != nil {
		return store.Restore{}, err
	}
	cfg, err := a.config(target)
	if err != nil {
		return store.Restore{}, err
	}
	if in.NewDatabase != "" {
		_, sess, err := a.open(ctx, target.OrgID, target.ID)
		if err != nil {
			return store.Restore{}, err
		}
		err = sess.CreateDatabase(ctx, in.NewDatabase)
		sess.Close(context.WithoutCancel(ctx))
		if err != nil {
			return store.Restore{}, &Error{Status: 422, Message: err.Error()}
		}
		a.audit(ctx, actor, "object.create_database", target.ID, map[string]string{"connection": target.Name, "database": in.NewDatabase})
		cfg.Database = in.NewDatabase
	}
	r := store.Restore{ID: secure.NewID("rst"), OrgID: b.OrgID, BackupID: b.ID, TargetID: target.ID, TargetName: target.Name,
		Database: database, Status: "running", CreatedBy: actor.User.Email}
	if err := a.Store.CreateRestore(ctx, r); err != nil {
		return r, err
	}
	a.audit(ctx, actor, "backup.restore", b.ID, map[string]any{"target": target.Name, "database": database, "clean": in.Clean, "tables": in.Tables})

	run := func() {
		bg := context.Background()
		out, err := func() (string, error) {
			src, err := a.openBackup(b)
			if err != nil {
				return "", err
			}
			defer src.Close()
			return backup.Restore(bg, b.Kind, b.Format, cfg, src, backup.RestoreOptions{Clean: in.Clean, KeepOwners: in.KeepOwners, Tables: in.Tables})
		}()
		if err != nil {
			a.Store.FinishRestore(bg, r.ID, "failed", err.Error(), out)
			return
		}
		a.Store.FinishRestore(bg, r.ID, "completed", "", out)
	}
	if !wait {
		go run()
		return r, nil
	}
	run()
	done, err := a.Store.GetRestore(ctx, r.OrgID, r.ID)
	if err != nil {
		return r, err
	}
	if done.Status != "completed" {
		return done, fmt.Errorf("%s", done.Error)
	}
	return done, nil
}
