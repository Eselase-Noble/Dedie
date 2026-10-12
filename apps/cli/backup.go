package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"dedie/engine/app"
	"dedie/engine/backup"
)

func (c *cli) backupCreate(args []string) error {
	fs := flag.NewFlagSet("backup create", flag.ExitOnError)
	note := fs.String("note", "", "a note stored with the backup")
	typ := fs.String("type", "full", "what to back up: full, schema, data, tables or physical")
	tables := fs.String("tables", "", "comma-separated tables for --type tables (schema.table on PostgreSQL)")
	level := fs.String("level", "full", "full, differential (changes since the last full backup) or incremental (changes since the last backup)")
	location := fs.String("location", "", "name of the backup location to write to (default: the data directory)")
	plain := fs.Bool("plain", false, "write the database's own format without encryption, readable directly from its path")
	conn, err := oneArg(parse(fs, args), "connection name")
	if err != nil {
		return err
	}
	fmt.Println("Backing up", conn, "...")
	b, err := c.be.CreateBackup(app.BackupInput{ConnectionID: conn, Note: *note, Type: *typ, Tables: splitList(*tables),
		Level: *level, LocationID: *location, Plain: *plain}, true)
	if err != nil {
		return err
	}
	if b.Status != "completed" {
		return fmt.Errorf("backup failed: %s", b.Error)
	}
	fmt.Printf("Backup %s completed: %s %s, %s, sha256 %s\n  %s\n", b.ID, backup.Method(b.Type), b.Level, humanBytes(b.SizeBytes), b.SHA256, b.Path)
	return nil
}

func (c *cli) backupList() error {
	list, err := c.be.ListBackups()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, b := range list {
		rows = append(rows, []string{b.ID, b.ConnectionName, b.Database, backup.Method(b.Type), b.Level, b.Type, b.Status, humanBytes(b.SizeBytes), b.CreatedAt.Local().Format("2006-01-02 15:04"), b.Note})
	}
	table("ID\tCONNECTION\tDATABASE\tMETHOD\tLEVEL\tCONTENTS\tSTATUS\tSIZE\tCREATED\tNOTE", rows)
	return nil
}

func (c *cli) backupVerify(args []string) error {
	id, err := oneArg(args, "backup id")
	if err != nil {
		return err
	}
	if err := c.be.VerifyBackup(id); err != nil {
		return err
	}
	fmt.Println("Backup is intact: the file matches its checksum and reads back completely.")
	return nil
}

func (c *cli) backupRestore(args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ExitOnError)
	var in app.RestoreInput
	fs.StringVar(&in.TargetID, "target", "", "connection to restore into")
	fs.StringVar(&in.NewDatabase, "new-database", "", "create this database on the target server and restore into it")
	tables := fs.String("tables", "", "restore only these comma-separated tables (PostgreSQL)")
	fs.BoolVar(&in.Clean, "clean", false, "drop existing objects before recreating them")
	fs.BoolVar(&in.KeepOwners, "keep-owners", false, "restore object ownership and privileges")
	fs.StringVar(&in.Confirm, "confirm", "", "the name of the database being restored into, to confirm")
	confirm := confirmFlag(fs)
	id, err := oneArg(parse(fs, args), "backup id")
	if err != nil {
		return err
	}
	in.Tables = splitList(*tables)
	fmt.Println("Restoring", id, "...")
	if err := c.confirmed(*confirm, func() error {
		_, err := c.be.RestoreBackup(id, in, true)
		return err
	}); err != nil {
		return err
	}
	fmt.Println("Restore completed.")
	return nil
}

func (c *cli) backupRemove(args []string) error {
	id, err := oneArg(args, "backup id")
	if err != nil {
		return err
	}
	if err := c.be.DeleteBackup(id); err != nil {
		return err
	}
	fmt.Println("Deleted", id)
	return nil
}

// backupExport writes the decrypted archive to a file for use outside Dedie.
func (c *cli) backupExport(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dedie backup export <id> <file>")
	}
	src, _, err := c.be.ExportBackup(args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.OpenFile(args[1], os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(args[1])
		return err
	}
	fmt.Printf("Wrote %s (%s). This copy is not encrypted; store it accordingly.\n", args[1], humanBytes(n))
	return nil
}

// backupExtract unpacks a physical backup into an empty data directory.
func (c *cli) backupExtract(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dedie backup extract <id> <directory>")
	}
	files, err := c.be.ExtractBackup(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Printf("Extracted %d files into %s.\nStart a database server of the same version on it:\n  PostgreSQL: pg_ctl -D %s -o \"-p 5433\" start\n  MySQL:      mysqld --datadir=%s --port=3307\n", files, args[1], args[1], args[1])
	return nil
}

func (c *cli) scheduleAdd(args []string) error {
	fs := flag.NewFlagSet("backup schedule add", flag.ExitOnError)
	in := app.ScheduleInput{Enabled: true}
	fs.StringVar(&in.Name, "name", "", "name of the schedule")
	fs.StringVar(&in.ConnectionID, "connection", "", "connection to back up")
	fs.StringVar(&in.Cron, "cron", "@daily", "when to run: minute hour day month weekday, or @hourly, @daily, @weekly, @monthly")
	fs.StringVar(&in.Type, "type", "full", "full, schema, data, tables or physical")
	tables := fs.String("tables", "", "comma-separated tables for --type tables")
	fs.StringVar(&in.Level, "level", "full", "full, differential or incremental")
	fs.StringVar(&in.LocationID, "location", "", "name of the backup location to write to")
	fs.BoolVar(&in.Plain, "plain", false, "write unencrypted files in the database's own format")
	fs.IntVar(&in.KeepLast, "keep-last", 7, "keep this many backups (0 keeps all)")
	fs.IntVar(&in.KeepDays, "keep-days", 0, "delete backups older than this many days (0 keeps all)")
	fs.BoolVar(&in.Verify, "verify", true, "verify each backup after it is written")
	parse(fs, args)
	in.Tables = splitList(*tables)
	sc, err := c.be.SaveSchedule("", in)
	if err != nil {
		return err
	}
	fmt.Printf("Schedule %s created; next run %s. A running dedie-server takes the backups.\n", sc.ID, sc.NextRunAt.Local().Format("2006-01-02 15:04"))
	return nil
}

func (c *cli) scheduleList() error {
	list, err := c.be.ListSchedules()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, sc := range list {
		next := "-"
		if sc.Enabled && !sc.NextRunAt.IsZero() {
			next = sc.NextRunAt.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, []string{sc.ID, sc.Name, sc.Cron, sc.Type, fmt.Sprintf("last %d / %d days", sc.KeepLast, sc.KeepDays), sc.LastStatus, next})
	}
	table("ID\tNAME\tCRON\tTYPE\tKEEP\tLAST RUN\tNEXT RUN", rows)
	return nil
}

func (c *cli) scheduleRemove(args []string) error {
	id, err := oneArg(args, "schedule id")
	if err != nil {
		return err
	}
	if err := c.be.DeleteSchedule(id); err != nil {
		return err
	}
	fmt.Println("Deleted", id)
	return nil
}

func (c *cli) locationAdd(args []string) error {
	fs := flag.NewFlagSet("backup location add", flag.ExitOnError)
	var in app.LocationInput
	fs.StringVar(&in.Name, "name", "", "name of the location")
	fs.StringVar(&in.Path, "path", "", "a directory on the machine that runs Dedie")
	fs.StringVar(&in.S3.Bucket, "s3-bucket", "", "store in this bucket of S3-compatible object storage instead of a directory")
	fs.StringVar(&in.S3.Endpoint, "s3-endpoint", "s3.amazonaws.com", "address of the storage service")
	fs.StringVar(&in.S3.Region, "s3-region", "", "region of the bucket")
	fs.StringVar(&in.S3.Prefix, "s3-prefix", "", "put objects under this prefix, like a folder")
	fs.StringVar(&in.S3.AccessKey, "s3-access-key", "", "access key; the secret key is prompted for or read from DEDIE_PASSWORD")
	parse(fs, args)
	if in.S3.Bucket != "" {
		in.Kind = "s3"
		secret, err := readPassword("Secret key: ")
		if err != nil {
			return err
		}
		in.S3.SecretKey = secret
	}
	l, err := c.be.CreateLocation(in)
	if err != nil {
		return err
	}
	fmt.Printf("Location %s added: %s\n", l.Name, l.Path)
	return nil
}

func (c *cli) locationList() error {
	def, list, err := c.be.ListLocations()
	if err != nil {
		return err
	}
	rows := [][]string{{"(default)", def, "-"}}
	for _, l := range list {
		rows = append(rows, []string{l.Name, l.Path, fmt.Sprint(l.Backups)})
	}
	table("NAME\tPATH\tBACKUPS", rows)
	return nil
}

func (c *cli) locationRemove(args []string) error {
	name, err := oneArg(args, "location name")
	if err != nil {
		return err
	}
	if err := c.be.DeleteLocation(name); err != nil {
		return err
	}
	fmt.Println("Removed", name, "(its directory and files were left as they are)")
	return nil
}

// backupRecover prepares a data directory that recovers a PostgreSQL
// server to a moment in time. It reads the log archive from this
// machine's data directory, so it runs on the Dedie server host.
func (c *cli) backupRecover(args []string) error {
	fs := flag.NewFlagSet("backup recover", flag.ExitOnError)
	to := fs.String("to", "", `moment to recover to, in local time: "2026-10-11 15:04:05" (default: the latest change archived)`)
	dir := fs.String("dir", "", "empty directory to build the recovered data directory in")
	pos := parse(fs, args)
	if len(pos) != 1 || *dir == "" {
		return errors.New(`usage: dedie backup recover <connection> --dir <directory> [--to "YYYY-MM-DD HH:MM:SS"]`)
	}
	if c.app == nil {
		return errors.New("recovery reads the log archive from the Dedie server's own disk; run this command on that machine (with --local if you are signed in to a server)")
	}
	var target time.Time
	if *to != "" {
		var err error
		if target, err = time.ParseInLocation("2006-01-02 15:04:05", *to, time.Local); err != nil {
			return errors.New(`write the moment as "YYYY-MM-DD HH:MM:SS"`)
		}
	}
	actor, err := c.app.LocalActor(c.ctx)
	if err != nil {
		return err
	}
	r, err := c.app.RecoverToTime(c.ctx, actor, pos[0], target, *dir)
	if err != nil {
		return err
	}
	moment := "the latest change archived"
	if !target.IsZero() {
		moment = target.Format("2006-01-02 15:04:05")
	}
	fmt.Printf("Prepared %s from the %s backup of %s (%d files).\n", r.Dir, r.Backup.Level, r.Backup.CreatedAt.Local().Format("2006-01-02 15:04:05"), r.Files)
	fmt.Printf("Start PostgreSQL of the same major version on it, on a free port:\n  pg_ctl -D %s -o \"-p 5433\" start\n", r.Dir)
	fmt.Printf("It replays the archived log up to %s and then opens for use. The original server is not touched.\n", moment)
	return nil
}
