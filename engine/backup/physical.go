package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"dedie/engine/connector"
)

// baseBackup writes a gzip-compressed tar of the server's data directory to
// w, taken with pg_basebackup while the server stays online. The archive
// includes the write-ahead log written during the copy, so it starts as a
// consistent server on its own. It covers every database of the server and
// needs an account with the REPLICATION attribute.
//
// With manifest set, only the blocks changed since the backup that manifest
// belongs to are written. That needs PostgreSQL 17 or newer with
// summarize_wal on, and the result is usable only together with the
// backups it builds on.
func baseBackup(ctx context.Context, cfg connector.Config, manifest string, w io.Writer) error {
	minMajor := 0
	args := []string{"--pgdata=-", "--format=tar", "--wal-method=fetch", "--checkpoint=fast", "--no-password", "--label=dedie"}
	if manifest != "" {
		minMajor = 17
		args = append(args, "--incremental="+manifest)
	}
	path, err := findTool("pg_basebackup", minMajor)
	if err != nil {
		return err
	}
	cmd, cleanup, err := pgCommand(ctx, path, cfg, args...)
	if err != nil {
		return err
	}
	defer cleanup()
	zw := gzip.NewWriter(w)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = zw, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := tail(&stderr)
		if manifest != "" && (strings.Contains(msg, "summariz") || strings.Contains(msg, "incremental")) {
			msg += ". Differential and incremental backups need PostgreSQL 17 or newer with summarize_wal = on, set before the full backup was taken"
		}
		return fmt.Errorf("pg_basebackup failed: %s", msg)
	}
	return zw.Close()
}

// ExtractManifest copies the backup manifest out of a physical backup
// read from r. The next differential or incremental backup is taken
// against it.
func ExtractManifest(r io.Reader, w io.Writer) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the backup holds no manifest, so nothing can be built on it")
		}
		if err != nil {
			return err
		}
		if filepath.Clean(hdr.Name) == "backup_manifest" {
			_, err := io.Copy(w, tr)
			return err
		}
	}
}

// CombineBackups rebuilds a complete data directory in out from a full
// backup and the differential or incremental backups taken after it, each
// already extracted into its own directory and given oldest first.
func CombineBackups(ctx context.Context, dirs []string, out string) error {
	path, err := findTool("pg_combinebackup", 17)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, append(append([]string{}, dirs...), "--output="+out)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_combinebackup failed: %s", tail(&stderr))
	}
	return os.Chmod(out, 0o700)
}

// ExtractPhysical unpacks a physical backup read from r into dir, which
// must be empty or absent. The result is a PostgreSQL data directory:
// start a server of the same major version on it to recover.
func ExtractPhysical(r io.Reader, dir string) (files int, err error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return 0, fmt.Errorf("%s is not empty; a data directory must be restored into an empty one", dir)
	}
	// PostgreSQL refuses to start on a data directory others can read.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return 0, err
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		// Never write outside the target directory, whatever the archive says.
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return files, fmt.Errorf("the archive contains an unsafe path: %s", hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return files, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return files, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return files, err
			}
			if err := f.Close(); err != nil {
				return files, err
			}
			files++
		case tar.TypeSymlink:
			// Tablespace links point outside the data directory; they are
			// not recreated, and a server with tablespaces is refused
			// earlier by pg_basebackup's tar-to-stdout mode.
			return files, fmt.Errorf("the archive contains a link (%s), which is not restored", hdr.Name)
		}
	}
}
