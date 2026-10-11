// Package storage is where backup files are kept: a directory on the
// machine that runs Dedie, or a bucket in S3-compatible object storage.
// The engine writes and reads backups through this interface and does not
// otherwise care which it is.
package storage

import (
	"context"
	"io"
)

// Kinds of location.
const (
	KindDir = "dir"
	KindS3  = "s3"
)

// Store keeps files under names it hands back as paths. A path is what is
// recorded with a backup and shown to people: a file path for a
// directory, an s3:// address for a bucket.
type Store interface {
	// Create starts a new file. The data is committed by Close and
	// discarded by Abort.
	Create(ctx context.Context, name string) (Writer, error)
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	Remove(ctx context.Context, path string) error
	Exists(ctx context.Context, path string) bool
	// Check proves the location can be written to, by writing and
	// removing a small file.
	Check(ctx context.Context) error
}

// Writer receives a file's contents.
type Writer interface {
	io.Writer
	// Close commits the file and reports where it is.
	Close() error
	// Abort discards what was written.
	Abort()
	// Path is the address of the file being written.
	Path() string
}
