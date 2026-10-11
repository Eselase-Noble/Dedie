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
	"path/filepath"
	"strings"

	"dedie/engine/connector"
)

// baseBackup writes a gzip-compressed tar of the server's data directory to
// w, taken with pg_basebackup while the server stays online. The archive
// includes the write-ahead log written during the copy, so it starts as a
// consistent server on its own. It covers every database of the server and
// needs an account with the REPLICATION attribute.
func baseBackup(ctx context.Context, cfg connector.Config, w io.Writer) error {
	path, err := findTool("pg_basebackup", 0)
	if err != nil {
		return err
	}
	cmd, cleanup, err := pgCommand(ctx, path, cfg, "--pgdata=-", "--format=tar", "--wal-method=fetch",
		"--checkpoint=fast", "--no-password", "--label=dedie")
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
		return fmt.Errorf("pg_basebackup failed: %s", tail(&stderr))
	}
	return zw.Close()
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
