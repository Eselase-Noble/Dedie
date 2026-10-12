package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dedie/engine/backup"
	"dedie/engine/connector"
	"dedie/engine/secure"
	"dedie/engine/store"
)

func backupAAD(orgID, backupID string) []byte { return []byte("backup/" + orgID + "/" + backupID) }

// backupsLink is the page that lists backups, restores and schedules.
const backupsLink = "/backups/"

// notifyVerifyFailed tells the organization that a backup can no longer be
// trusted. It is said once per backup, however often the check is repeated.
func (a *App) notifyVerifyFailed(ctx context.Context, b store.Backup, err error) {
	a.Notify(ctx, store.Notification{OrgID: b.OrgID, Kind: NoteBackupVerifyFailed, Level: LevelError,
		Title: "Backup failed verification: " + b.Database + " on " + b.ConnectionName,
		Body:  "The backup taken on " + b.CreatedAt.Local().Format("2 January 2006 at 15:04") + " cannot be relied on: " + firstLine(err) + ".",
		Link:  backupsLink, ConnectionID: b.ConnectionID, Once: "verify/" + b.ID})
}

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
	// Level is full (the default), differential or incremental.
	Level string `json:"level"`
	// LocationID names where the file is written; empty is the data
	// directory.
	LocationID string `json:"location_id"`
	// Plain writes the backup in the database's own format, without
	// Dedie's encryption, so that the database's tools can read the file
	// where it lies. Protecting it is then up to whoever owns the directory.
	Plain bool `json:"plain"`
}

// errNoBase is returned when a differential or incremental backup has
// nothing to build on.
var errNoBase = invalid("there is no full backup of this connection to build on; take a full backup first")

// CreateBackup starts a backup of a connection's database and returns
// immediately; the backup's status moves from "running" to "completed" or
// "failed". With wait set the call blocks until it finishes.
func (a *App) CreateBackup(ctx context.Context, actor Actor, in BackupInput, wait bool) (store.Backup, error) {
	if err := actor.require(PermBackup); err != nil {
		return store.Backup{}, err
	}
	return a.startBackup(ctx, actor, in, "", wait)
}

// baseFor finds the backup a differential or incremental backup of a
// connection builds on: the newest full backup for a differential, the
// newest backup of the same family for an incremental.
func (a *App) baseFor(ctx context.Context, c store.Connection, typ, level string) (store.Backup, error) {
	list, err := a.Store.ConnectionBackups(ctx, c.OrgID, c.ID)
	if err != nil {
		return store.Backup{}, err
	}
	for _, b := range list {
		if b.Type != typ || (level == backup.LevelDifferential && b.Level != backup.LevelFull) {
			continue
		}
		// A MySQL backup can only be continued from a recorded position.
		if c.Kind == "mysql" && b.Position == "" {
			continue
		}
		if st, err := a.backupStore(ctx, b); err != nil || !st.Exists(ctx, b.Path) {
			continue
		}
		return b, nil
	}
	return store.Backup{}, errNoBase
}

// chain returns a backup together with everything it builds on, oldest
// first: the full backup, then each later one in order.
func (a *App) chain(ctx context.Context, b store.Backup) ([]store.Backup, error) {
	out := []store.Backup{b}
	for cur := b; cur.ParentID != ""; {
		parent, err := a.Store.GetBackup(ctx, b.OrgID, cur.ParentID)
		if err != nil {
			return nil, conflict("a backup this one builds on has been deleted, so it can no longer be restored")
		}
		if parent.Status != "completed" {
			return nil, conflict("a backup this one builds on is %s", parent.Status)
		}
		out = append([]store.Backup{parent}, out...)
		cur = parent
		if len(out) > 1000 {
			return nil, conflict("the chain of backups is too long")
		}
	}
	return out, nil
}

// canRecordPosition reports whether a MySQL account holds the privileges
// mysqldump needs to note the binary log position in a dump.
func canRecordPosition(ctx context.Context, sess connector.Session) bool {
	res, err := sess.Query(ctx, "SHOW GRANTS", connector.QueryOptions{ReadOnly: true, MaxRows: 200})
	if err != nil || len(res) == 0 {
		return false
	}
	var reload, client bool
	for _, row := range res[0].Rows {
		if len(row) == 0 || row[0] == nil {
			continue
		}
		grant := strings.ToUpper(*row[0])
		// Global privileges are granted ON *.*; anything narrower does
		// not include these two.
		if !strings.Contains(grant, " ON *.*") {
			continue
		}
		if strings.Contains(grant, "ALL PRIVILEGES") {
			return true
		}
		reload = reload || strings.Contains(grant, "RELOAD")
		client = client || strings.Contains(grant, "REPLICATION CLIENT")
	}
	return reload && client
}

// startBackup does the work of CreateBackup for a user or for a schedule.
func (a *App) startBackup(ctx context.Context, actor Actor, in BackupInput, scheduleID string, wait bool) (store.Backup, error) {
	if in.Type == "" {
		in.Type = backup.TypeFull
	}
	if in.Level == "" {
		in.Level = backup.LevelFull
	}
	c, sess, err := a.open(ctx, actor, in.ConnectionID)
	if err != nil {
		return store.Backup{}, err
	}
	info, err := sess.Info(ctx)
	recordPosition := err == nil && c.Kind == "mysql" && in.Type == backup.TypeFull && canRecordPosition(ctx, sess)
	sess.Close(context.WithoutCancel(ctx))
	if err != nil {
		return store.Backup{}, err
	}
	if !backup.Supported(c.Kind) {
		return store.Backup{}, invalid("backups are not supported for %s connections yet", c.Kind)
	}
	opts := backup.Options{Type: in.Type, Tables: in.Tables, Level: in.Level, RecordPosition: recordPosition}
	if in.Type != backup.TypeTables {
		opts.Tables = nil
	}
	if err := backup.CheckOptions(c.Kind, opts); err != nil {
		return store.Backup{}, invalid("%s", err.Error())
	}

	var base store.Backup
	note := in.Note
	switch in.Level {
	case backup.LevelFull:
	case backup.LevelDifferential, backup.LevelIncremental:
		if !backup.LevelsSupported(c.Kind, in.Type) {
			if c.Kind == "postgres" {
				return store.Backup{}, invalid("PostgreSQL takes differential and incremental backups physically; choose a physical backup. A logical backup is always full")
			}
			return store.Backup{}, invalid("MySQL takes differential and incremental backups of the whole database; choose a full logical backup as the type")
		}
		base, err = a.baseFor(ctx, c, in.Type, in.Level)
		if errors.Is(err, errNoBase) && scheduleID != "" {
			// A schedule's first run has nothing to build on. It takes
			// the full backup the later runs will need.
			opts.Level, in.Level = backup.LevelFull, backup.LevelFull
			note = strings.TrimSpace(note + " (full: there was no backup to build on)")
			break
		}
		if err != nil {
			if errors.Is(err, errNoBase) && c.Kind == "mysql" {
				return store.Backup{}, invalid("there is no full backup of this connection with a recorded binary log position. Take a full backup first, with an account that has RELOAD and REPLICATION CLIENT")
			}
			return store.Backup{}, err
		}
		opts.From = base.Position
	default:
		return store.Backup{}, invalid("level must be full, differential or incremental")
	}

	target, loc, err := a.locationStore(ctx, c.OrgID, in.LocationID)
	if err != nil {
		return store.Backup{}, err
	}
	in.LocationID = loc.ID
	cfg, err := a.config(c)
	if err != nil {
		return store.Backup{}, err
	}
	b := store.Backup{ID: secure.NewID("bak"), OrgID: c.OrgID, ConnectionID: c.ID, ConnectionName: c.Name, Database: c.Database,
		Kind: c.Kind, Type: opts.Type, Format: backup.LevelFormat(c.Kind, opts.Type, in.Level), Tables: opts.Tables, ScheduleID: scheduleID,
		Level: in.Level, ParentID: base.ID, Encrypted: !in.Plain, LocationID: in.LocationID,
		ServerVersion: info.Version, Status: "running", Note: note, CreatedBy: actor.User.Email}
	// The name says what the file is to someone looking at the directory;
	// the id keeps two backups taken in the same second apart.
	ext := ".dediebak"
	if in.Plain {
		ext = backup.Extension(b.Format)
	}
	name := fmt.Sprintf("%s-%s-%s-%s-%s%s", safeFileName(c.Database), time.Now().UTC().Format("20060102-150405"),
		backup.Method(b.Type), b.Level, strings.TrimPrefix(b.ID, "bak_")[:8], ext)
	// Opening the file first means a location that cannot be written to
	// is reported at once, not as a failed backup later.
	file, err := target.Create(context.WithoutCancel(ctx), name)
	if err != nil {
		return store.Backup{}, invalid("the backup location cannot be written to: %s", err.Error())
	}
	b.Path = file.Path()
	if err := a.Store.CreateBackup(ctx, b); err != nil {
		file.Abort()
		return b, err
	}
	a.audit(ctx, actor, "backup.create", b.ID, map[string]any{"connection": c.Name, "database": c.Database, "type": b.Type,
		"level": b.Level, "tables": b.Tables, "path": b.Path, "encrypted": b.Encrypted})

	run := func() {
		bg := context.Background()
		size, sum, meta, err := func() (int64, string, backup.Meta, error) {
			var meta backup.Meta
			if base.ID != "" && c.Kind == "postgres" {
				// PostgreSQL works out what changed from the manifest of
				// the backup being built on.
				manifest, err := a.manifestOf(base)
				if err != nil {
					return 0, "", meta, err
				}
				defer os.Remove(manifest)
				opts.ManifestPath = manifest
			}
			// The digest covers the file as stored, so integrity can be
			// checked later without the key.
			hash := sha256.New()
			count := &countWriter{}
			var out io.Writer = io.MultiWriter(file, hash, count)
			var enc *secure.StreamWriter
			var err error
			if b.Encrypted {
				if enc, err = secure.NewStreamWriter(a.keyring, out, backupAAD(b.OrgID, b.ID)); err != nil {
					return 0, "", meta, err
				}
				out = enc
			}
			if meta, err = backup.Dump(bg, c.Kind, cfg, info.VersionNum, opts, out); err != nil {
				return 0, "", meta, err
			}
			if enc != nil {
				if err := enc.Close(); err != nil {
					return 0, "", meta, err
				}
			}
			// Close commits the file: flushed to disk, or confirmed by
			// the object store.
			if err := file.Close(); err != nil {
				return 0, "", meta, err
			}
			return count.n, hex.EncodeToString(hash.Sum(nil)), meta, nil
		}()
		if err != nil {
			file.Abort()
			a.Store.FinishBackup(bg, b.ID, "failed", err.Error(), 0, "")
			// A schedule reports its own failures, with its name.
			if scheduleID == "" {
				a.emit(b.OrgID, Event{Type: EventBackupFailed, Subject: "Backup of " + c.Name + " failed", Message: err.Error()})
				a.Notify(bg, store.Notification{OrgID: b.OrgID, Kind: NoteBackupFailed, Level: LevelError, Title: "Backup failed: " + c.Name,
					Body: firstLine(err), Link: backupsLink, ConnectionID: c.ID})
			}
			return
		}
		if meta.Position != "" {
			a.Store.SetBackupPosition(bg, b.ID, meta.Position)
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

// countWriter counts the bytes written through it.
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// manifestOf writes the manifest of a physical backup to a temporary file
// and returns its path. The caller removes it.
func (a *App) manifestOf(b store.Backup) (string, error) {
	src, err := a.openBackup(b)
	if err != nil {
		return "", err
	}
	defer src.Close()
	f, err := os.CreateTemp("", "dedie-manifest-*")
	if err != nil {
		return "", err
	}
	if err := backup.ExtractManifest(src, f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
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
	if n, err := a.Store.BackupDependents(ctx, b.ID); err != nil {
		return err
	} else if n > 0 {
		return conflict("%d later backup(s) build on this one and could not be restored without it; delete those first", n)
	}
	if err := a.removeBackupFile(ctx, b); err != nil {
		return err
	}
	if err := a.Store.DeleteBackup(ctx, b.OrgID, b.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "backup.delete", b.ID, map[string]string{"connection": b.ConnectionName, "database": b.Database})
	return nil
}

// removeBackupFile deletes a backup's file from wherever it is stored.
func (a *App) removeBackupFile(ctx context.Context, b store.Backup) error {
	if b.Path == "" {
		return nil
	}
	st, err := a.backupStore(ctx, b)
	if err != nil {
		return err
	}
	return st.Remove(ctx, b.Path)
}

// rawBackup opens a backup's file as it is stored.
func (a *App) rawBackup(b store.Backup) (io.ReadCloser, error) {
	st, err := a.backupStore(context.Background(), b)
	if err != nil {
		return nil, err
	}
	f, err := st.Open(context.Background(), b.Path)
	if err != nil {
		return nil, conflict("the backup file is missing or cannot be read: %s", b.Path)
	}
	return f, nil
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
		if b.Status == "completed" {
			a.notifyVerifyFailed(ctx, b, err)
		}
		return err
	}
	return a.Store.MarkBackupVerified(ctx, b.ID)
}

// openBackup returns the decrypted contents of a completed backup.
func (a *App) openBackup(b store.Backup) (io.ReadCloser, error) {
	if b.Status != "completed" {
		return nil, conflict("the backup is %s", b.Status)
	}
	f, err := a.rawBackup(b)
	if err != nil {
		return nil, err
	}
	if !b.Encrypted {
		return f, nil
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
// machine, ready for PostgreSQL to be started on it. For a differential or
// incremental backup the full backup and everything between are unpacked
// too and combined into one complete data directory.
func (a *App) ExtractBackup(ctx context.Context, actor Actor, id, dir string) (int, error) {
	if err := actor.require(PermRestore); err != nil {
		return 0, err
	}
	b, err := a.Store.GetBackup(ctx, actor.User.OrgID, id)
	if err != nil {
		return 0, wrap(err)
	}
	if !backup.Physical(b.Format) {
		return 0, invalid("only physical backups are extracted; restore this one into a connection")
	}
	chain, err := a.chain(ctx, b)
	if err != nil {
		return 0, err
	}
	extract := func(part store.Backup, into string) (int, error) {
		if err := a.verifyBackupFile(part); err != nil {
			return 0, err
		}
		r, err := a.openBackup(part)
		if err != nil {
			return 0, err
		}
		defer r.Close()
		return backup.ExtractPhysical(r, into)
	}
	var files int
	if len(chain) == 1 {
		if files, err = extract(b, dir); err != nil {
			return files, invalid("%s", err.Error())
		}
	} else {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return 0, invalid("%s is not empty; a data directory must be restored into an empty one", dir)
		}
		parts, err := os.MkdirTemp(filepath.Dir(filepath.Clean(dir)), ".dedie-restore-*")
		if err != nil {
			return 0, invalid("%s", err.Error())
		}
		defer os.RemoveAll(parts)
		var dirs []string
		for i, part := range chain {
			into := filepath.Join(parts, fmt.Sprintf("%03d", i))
			if _, err := extract(part, into); err != nil {
				return 0, invalid("%s", err.Error())
			}
			dirs = append(dirs, into)
		}
		// pg_combinebackup creates the output directory itself.
		os.Remove(dir)
		if err := backup.CombineBackups(ctx, dirs, dir); err != nil {
			return 0, invalid("%s", err.Error())
		}
		filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files++
			}
			return nil
		})
	}
	a.audit(ctx, actor, "backup.extract", b.ID, map[string]any{"connection": b.ConnectionName, "directory": dir, "backups": len(chain)})
	return files, nil
}

func (a *App) verifyBackupFile(b store.Backup) error {
	if b.Status != "completed" {
		return conflict("the backup is %s", b.Status)
	}
	f, err := a.rawBackup(b)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	var body io.Reader = io.TeeReader(f, hash)
	if b.Encrypted {
		// Reading the whole archive through the cipher proves every
		// chunk is intact and in place.
		if body, err = secure.NewStreamReader(a.keyring, body, backupAAD(b.OrgID, b.ID)); err != nil {
			return conflict("%s", err.Error())
		}
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
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
	if backup.Physical(b.Format) {
		return store.Restore{}, invalid("a physical backup is a copy of the server's data files and is not restored into a running database; extract it with `dedie backup extract %s <directory>` and start the database server on that directory", b.ID)
	}
	target, err := a.conn(ctx, actor, in.TargetID)
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
	// A restore into the connection's own database writes over what is
	// there. Into a new database on the same server it does not.
	if in.NewDatabase == "" {
		if err := a.confirmProduction(ctx, actor, target, "backup.restore", "restore a backup over it"); err != nil {
			return store.Restore{}, err
		}
	}
	chain, err := a.chain(ctx, b)
	if err != nil {
		return store.Restore{}, err
	}
	if len(chain) > 1 && database != b.Database {
		// The changes in a differential or incremental backup name the
		// database they were made in.
		return store.Restore{}, invalid("this %s backup can only be restored into a database named %s, as the source was; restore the full backup on its own to use another name", b.Level, b.Database)
	}
	if len(in.Tables) > 0 && b.Format != backup.FormatPGCustom && !(b.Format == "" && b.Kind == "postgres") {
		return store.Restore{}, invalid("restoring selected tables is available for PostgreSQL backups only")
	}
	for _, part := range chain {
		if err := a.verifyBackupFile(part); err != nil {
			return store.Restore{}, err
		}
	}
	cfg, err := a.config(target)
	if err != nil {
		return store.Restore{}, err
	}
	if in.NewDatabase != "" {
		_, sess, err := a.open(ctx, actor, target.ID)
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
		// The full backup is applied first, then each later one in the
		// order it was taken.
		out, err := func() (string, error) {
			var log []string
			for i, part := range chain {
				src, err := a.openBackup(part)
				if err != nil {
					return strings.Join(log, "\n"), err
				}
				text, err := backup.Restore(bg, part.Kind, part.Format, cfg, src, backup.RestoreOptions{Clean: in.Clean, KeepOwners: in.KeepOwners, Tables: in.Tables})
				src.Close()
				if len(chain) > 1 {
					text = strings.TrimSpace(fmt.Sprintf("[%d of %d] %s backup of %s\n%s", i+1, len(chain), part.Level, part.CreatedAt.Local().Format("2006-01-02 15:04:05"), text))
				}
				log = append(log, text)
				if err != nil {
					return strings.Join(log, "\n"), err
				}
			}
			return strings.Join(log, "\n"), nil
		}()
		if err != nil {
			a.Store.FinishRestore(bg, r.ID, "failed", err.Error(), out)
			a.Notify(bg, store.Notification{OrgID: r.OrgID, Kind: NoteRestoreFailed, Level: LevelError, Title: "Restore failed: " + database + " on " + target.Name,
				Body: firstLine(err), Link: backupsLink, ConnectionID: target.ID})
			return
		}
		a.Store.FinishRestore(bg, r.ID, "completed", "", out)
		a.Notify(bg, store.Notification{OrgID: r.OrgID, Kind: NoteRestoreCompleted, Level: LevelSuccess, Title: "Restore finished: " + database + " on " + target.Name,
			Body: "The backup of " + b.Database + " taken on " + b.CreatedAt.Local().Format("2 January 2006 at 15:04") + " was restored.", Link: backupsLink, ConnectionID: target.ID})
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
