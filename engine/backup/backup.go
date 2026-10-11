package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"dedie/engine/connector"
)

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
}

// Supported reports whether backups exist for a database kind.
func Supported(kind string) bool { return kind == "postgres" || kind == "mysql" }

// Dump writes a logical backup of the database to w. serverVersion is the
// connector's numeric server version.
func Dump(ctx context.Context, kind string, cfg connector.Config, serverVersion int, w io.Writer) error {
	switch kind {
	case "postgres":
		return dumpPostgres(ctx, cfg, serverVersion/10000, w)
	case "mysql":
		return dumpMySQL(ctx, cfg, w)
	}
	return fmt.Errorf("backups are not supported for %s", kind)
}

// Restore applies a backup read from r to the database and returns the
// restoring tool's own output.
func Restore(ctx context.Context, kind string, cfg connector.Config, r io.Reader, opts RestoreOptions) (string, error) {
	switch kind {
	case "postgres":
		return restorePostgres(ctx, cfg, r, opts)
	case "mysql":
		return restoreMySQL(ctx, cfg, r)
	}
	return "", fmt.Errorf("restores are not supported for %s", kind)
}

// tail keeps the end of a tool's stderr for the operator.
func tail(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if len(s) > 4000 {
		s = "..." + s[len(s)-4000:]
	}
	return s
}
