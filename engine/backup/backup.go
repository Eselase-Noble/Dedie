package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"dedie/engine/connector"
)

// Backup types. A backup holds the whole database unless narrowed.
const (
	TypeFull     = "full"     // structure and data
	TypeSchema   = "schema"   // structure only
	TypeData     = "data"     // rows only, for loading into an existing structure
	TypeTables   = "tables"   // selected tables, structure and data
	TypePhysical = "physical" // a copy of the server's data files (PostgreSQL)
)

// Backup levels say how much of the database's history a backup holds.
const (
	// LevelFull stands on its own.
	LevelFull = "full"
	// LevelDifferential holds what changed since the last full backup;
	// restoring needs that full backup and this one.
	LevelDifferential = "differential"
	// LevelIncremental holds what changed since the last backup of any
	// level; restoring needs the full backup and every one after it.
	LevelIncremental = "incremental"
)

// Methods. A logical backup is a description of the data that any server
// of the kind can load. A physical backup is a copy of the server's own
// files, restored as a whole server.
const (
	MethodLogical  = "logical"
	MethodPhysical = "physical"
)

// Method reports how a backup of the given type is taken.
func Method(typ string) string {
	if typ == TypePhysical {
		return MethodPhysical
	}
	return MethodLogical
}

// LevelsSupported reports whether differential and incremental backups
// exist for a kind of database and a backup type. PostgreSQL offers them
// for physical backups (version 17 and newer); MySQL for full logical
// backups, as the binary log written since the backup they build on.
func LevelsSupported(kind, typ string) bool {
	return kind == "postgres" && typ == TypePhysical || kind == "mysql" && typ == TypeFull
}

// Archive formats, recorded with each backup so that it is restored the way
// it was written.
const (
	FormatPGCustom = "pgcustom"      // pg_dump custom format
	FormatSQL      = "sql"           // plain SQL, written by releases before gzip was added
	FormatSQLGzip  = "sql.gz"        // gzip-compressed SQL
	FormatPGBase   = "pgbase.tgz"    // gzip-compressed tar of a PostgreSQL data directory
	FormatBinlog   = "binlog.sql.gz" // gzip-compressed MySQL binary log events as SQL
)

// Options select what a backup contains.
type Options struct {
	Type string
	// Tables lists the tables of a TypeTables backup, as schema.table for
	// PostgreSQL and as plain table names for MySQL.
	Tables []string
	// Level is LevelFull unless the backup builds on an earlier one.
	Level string
	// ManifestPath is the manifest file of the backup a PostgreSQL
	// differential or incremental backup builds on.
	ManifestPath string
	// From is the binary log position a MySQL differential or incremental
	// backup starts at: where the backup it builds on ended.
	From string
	// RecordPosition makes a full MySQL backup note the binary log
	// position it was taken at, which later backups can build on. It
	// needs the RELOAD and REPLICATION CLIENT privileges.
	RecordPosition bool
}

// Meta is what a finished backup reports about itself.
type Meta struct {
	// Position is where a MySQL backup ended in the binary log, as
	// "file:offset". Empty when it was not recorded.
	Position string
}

// RestoreOptions control how an archive is applied.
type RestoreOptions struct {
	// Clean drops each object before recreating it, replacing what is in
	// the target database. MySQL dumps always replace tables of the same
	// name, so the option has no further effect there.
	Clean bool
	// KeepOwners restores ownership and privileges as recorded
	// (PostgreSQL). Leave it off when the target server does not have the
	// same roles.
	KeepOwners bool
	// Tables restores only these tables from a PostgreSQL archive.
	Tables []string
}

// Supported reports whether backups exist for a database kind.
func Supported(kind string) bool { return kind == "postgres" || kind == "mysql" }

// Types lists the backup types available for a database kind.
func Types(kind string) []string {
	switch kind {
	case "postgres":
		return []string{TypeFull, TypeSchema, TypeData, TypeTables, TypePhysical}
	case "mysql":
		return []string{TypeFull, TypeSchema, TypeData, TypeTables}
	}
	return nil
}

// Format is the archive format a backup of this kind and type is written in.
func Format(kind, typ string) string {
	switch {
	case kind == "postgres" && typ == TypePhysical:
		return FormatPGBase
	case kind == "postgres":
		return FormatPGCustom
	}
	return FormatSQLGzip
}

// LevelFormat is Format for a backup of the given level.
func LevelFormat(kind, typ, level string) string {
	if kind == "mysql" && level != "" && level != LevelFull {
		return FormatBinlog
	}
	return Format(kind, typ)
}

// Extension is the file name suffix for a downloaded archive.
func Extension(format string) string {
	switch format {
	case FormatPGCustom:
		return ".dump"
	case FormatSQLGzip:
		return ".sql.gz"
	case FormatPGBase:
		return ".tar.gz"
	case FormatBinlog:
		return ".binlog.sql.gz"
	}
	return ".sql"
}

// CheckOptions validates a backup request before anything is started.
func CheckOptions(kind string, opts Options) error {
	ok := false
	for _, t := range Types(kind) {
		ok = ok || t == opts.Type
	}
	if !ok {
		return fmt.Errorf("%s backups are not available for %s", opts.Type, kind)
	}
	if opts.Type == TypeTables && len(opts.Tables) == 0 {
		return fmt.Errorf("choose at least one table")
	}
	for _, t := range opts.Tables {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, "\x00\n\r") {
			return fmt.Errorf("%q is not a valid table name", t)
		}
	}
	return nil
}

// Dump writes a backup of the database to w. serverVersion is the
// connector's numeric server version.
func Dump(ctx context.Context, kind string, cfg connector.Config, serverVersion int, opts Options, w io.Writer) (Meta, error) {
	if err := CheckOptions(kind, opts); err != nil {
		return Meta{}, err
	}
	leveled := opts.Level != "" && opts.Level != LevelFull
	if leveled && !LevelsSupported(kind, opts.Type) {
		return Meta{}, fmt.Errorf("%s backups are not available for this kind of backup", opts.Level)
	}
	switch kind {
	case "postgres":
		if opts.Type == TypePhysical {
			return Meta{}, baseBackup(ctx, cfg, opts.ManifestPath, w)
		}
		return Meta{}, dumpPostgres(ctx, cfg, serverVersion/10000, opts, w)
	case "mysql":
		if leveled {
			return binlogBackup(ctx, cfg, opts.From, w)
		}
		return dumpMySQL(ctx, cfg, opts, w)
	}
	return Meta{}, fmt.Errorf("backups are not supported for %s", kind)
}

// Restore applies a backup read from r to the database and returns the
// restoring tool's own output. format is the one recorded with the backup;
// empty means a backup from before formats were recorded.
func Restore(ctx context.Context, kind, format string, cfg connector.Config, r io.Reader, opts RestoreOptions) (string, error) {
	if format == "" {
		format = map[string]string{"postgres": FormatPGCustom, "mysql": FormatSQL}[kind]
	}
	switch format {
	case FormatPGCustom:
		return restorePostgres(ctx, cfg, r, opts)
	case FormatSQL:
		return restoreMySQL(ctx, cfg, r)
	case FormatSQLGzip, FormatBinlog:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return "", err
		}
		defer zr.Close()
		return restoreMySQL(ctx, cfg, zr)
	case FormatPGBase:
		return "", fmt.Errorf("a physical backup is a copy of the server's data files; extract it into an empty data directory with `dedie backup extract` and start PostgreSQL on it")
	}
	return "", fmt.Errorf("unknown backup format %q", format)
}

// tail keeps the end of a tool's stderr for the operator.
func tail(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if len(s) > 4000 {
		s = "..." + s[len(s)-4000:]
	}
	return s
}
