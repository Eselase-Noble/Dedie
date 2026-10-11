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

// Archive formats, recorded with each backup so that it is restored the way
// it was written.
const (
	FormatPGCustom = "pgcustom"   // pg_dump custom format
	FormatSQL      = "sql"        // plain SQL, written by releases before gzip was added
	FormatSQLGzip  = "sql.gz"     // gzip-compressed SQL
	FormatPGBase   = "pgbase.tgz" // gzip-compressed tar of a PostgreSQL data directory
)

// Options select what a backup contains.
type Options struct {
	Type string
	// Tables lists the tables of a TypeTables backup, as schema.table for
	// PostgreSQL and as plain table names for MySQL.
	Tables []string
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

// Extension is the file name suffix for a downloaded archive.
func Extension(format string) string {
	switch format {
	case FormatPGCustom:
		return ".dump"
	case FormatSQLGzip:
		return ".sql.gz"
	case FormatPGBase:
		return ".tar.gz"
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
func Dump(ctx context.Context, kind string, cfg connector.Config, serverVersion int, opts Options, w io.Writer) error {
	if err := CheckOptions(kind, opts); err != nil {
		return err
	}
	switch kind {
	case "postgres":
		if opts.Type == TypePhysical {
			return baseBackup(ctx, cfg, w)
		}
		return dumpPostgres(ctx, cfg, serverVersion/10000, opts, w)
	case "mysql":
		return dumpMySQL(ctx, cfg, opts, w)
	}
	return fmt.Errorf("backups are not supported for %s", kind)
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
	case FormatSQLGzip:
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
