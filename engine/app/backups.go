package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dedie/engine/backup"
	"dedie/engine/connector/postgres"
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

// CreateBackup starts a logical backup of a connection's database and returns
// immediately; the backup's status moves from "running" to "completed" or
// "failed". With wait set the call blocks until it finishes.
func (a *App) CreateBackup(ctx context.Context, actor Actor, connID, note string, wait bool) (store.Backup, error) {
	if err := actor.require(PermBackup); err != nil {
		return store.Backup{}, err
	}
	c, sess, err := a.open(ctx, actor.User.OrgID, connID)
	if err != nil {
		return store.Backup{}, err
	}
	info, err := sess.Info(ctx)
	sess.Close(context.WithoutCancel(ctx))
	if err != nil {
		return store.Backup{}, err
	}
	if c.Kind != postgres.Kind {
		return store.Backup{}, invalid("backups are supported for PostgreSQL connections only in this version")
	}
	cfg, err := a.config(c)
	if err != nil {
		return store.Backup{}, err
	}
	b := store.Backup{ID: secure.NewID("bak"), OrgID: c.OrgID, ConnectionID: c.ID, ConnectionName: c.Name, Database: c.Database,
		Kind: c.Kind, ServerVersion: info.Version, Status: "running", Note: note, CreatedBy: actor.User.Email}
	b.Path = filepath.Join(a.dataDir, "backups", b.ID+".dediebak")
	if err := a.Store.CreateBackup(ctx, b); err != nil {
		return b, err
	}
	a.audit(ctx, actor, "backup.create", b.ID, map[string]string{"connection": c.Name, "database": c.Database})

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
			if err := backup.Dump(bg, cfg, info.VersionNum/10000, enc); err != nil {
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
	return a.verifyBackupFile(b)
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

// RestoreInput describes a restore. Confirm must repeat the target database
// name: a restore with Clean replaces existing objects and cannot be undone.
type RestoreInput struct {
	TargetID   string `json:"target_id"`
	Clean      bool   `json:"clean"`
	KeepOwners bool   `json:"keep_owners"`
	Confirm    string `json:"confirm"`
}

// RestoreBackup verifies a backup and applies it to the target connection in
// one transaction. It returns once the restore has started unless wait is set.
func (a *App) RestoreBackup(ctx context.Context, actor Actor, backupID string, in RestoreInput, wait bool) (store.Restore, error) {
	if err := actor.require(PermRestore); err != nil {
		return store.Restore{}, err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, backupID)
	if err != nil {
		return store.Restore{}, wrap(err)
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
	if in.Confirm != target.Database {
		return store.Restore{}, invalid("type the target database name (%s) to confirm the restore", target.Database)
	}
	if err := a.verifyBackupFile(b); err != nil {
		return store.Restore{}, err
	}
	cfg, err := a.config(target)
	if err != nil {
		return store.Restore{}, err
	}
	r := store.Restore{ID: secure.NewID("rst"), OrgID: b.OrgID, BackupID: b.ID, TargetID: target.ID, TargetName: target.Name,
		Status: "running", CreatedBy: actor.User.Email}
	if err := a.Store.CreateRestore(ctx, r); err != nil {
		return r, err
	}
	a.audit(ctx, actor, "backup.restore", b.ID, map[string]any{"target": target.Name, "database": target.Database, "clean": in.Clean})

	run := func() {
		bg := context.Background()
		out, err := func() (string, error) {
			f, err := os.Open(b.Path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			dec, err := secure.NewStreamReader(a.keyring, f, backupAAD(b.OrgID, b.ID))
			if err != nil {
				return "", err
			}
			return backup.Restore(bg, cfg, dec, backup.RestoreOptions{Clean: in.Clean, KeepOwners: in.KeepOwners})
		}()
		if err != nil {
			a.Store.FinishRestore(bg, r.ID, "failed", err.Error(), out)
			return
		}
		a.Store.FinishRestore(bg, r.ID, "completed", "", out)
	}
	if wait {
		run()
		restores, err := a.Store.ListRestores(ctx, r.OrgID)
		if err != nil {
			return r, err
		}
		for _, done := range restores {
			if done.ID == r.ID {
				if done.Status != "completed" {
					return done, fmt.Errorf("%s", done.Error)
				}
				return done, nil
			}
		}
		return r, nil
	}
	go run()
	return r, nil
}
