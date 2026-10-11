package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// Dir stores files in a directory, readable by their owner only.
type Dir struct{ Path string }

type fileWriter struct {
	f    *os.File
	path string
}

func (w *fileWriter) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *fileWriter) Path() string                { return w.path }

func (w *fileWriter) Close() error {
	// Flushed to disk before the backup is reported as complete.
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

func (w *fileWriter) Abort() {
	w.f.Close()
	os.Remove(w.path)
}

func (d Dir) Create(ctx context.Context, name string) (Writer, error) {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(d.Path, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileWriter{f: f, path: path}, nil
}

// Open, Remove and Exists take the full path recorded with the backup,
// so a backup is still found after its location's settings change.
func (Dir) Open(ctx context.Context, path string) (io.ReadCloser, error) { return os.Open(path) }

func (Dir) Remove(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (Dir) Exists(ctx context.Context, path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (d Dir) Check(ctx context.Context) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(d.Path, ".dedie-write-test-*")
	if err != nil {
		return err
	}
	probe.Close()
	return os.Remove(probe.Name())
}
