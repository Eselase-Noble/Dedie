// Package app is the engine's application layer. Every front end (the HTTP
// server, the CLI and the desktop launcher) calls these methods and nothing
// below them, so authorization, auditing and secret handling are enforced in
// one place regardless of how a request arrives.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dedie/engine/secure"
	"dedie/engine/store"

	// Database drivers and migration pipelines register themselves on import.
	_ "dedie/engine/connector/mysql"
	_ "dedie/engine/connector/postgres"
	_ "dedie/engine/migrate/mymy"
	_ "dedie/engine/migrate/pgpg"
)

type App struct {
	Store   *store.Store
	keyring secure.Keyring
	dataDir string
	// instance identifies this process when leasing migration jobs.
	instance string
}

// DefaultDataDir is where state lives unless DEDIE_DATA_DIR or a flag says otherwise.
func DefaultDataDir() string {
	if dir := os.Getenv("DEDIE_DATA_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dedie"
	}
	return filepath.Join(home, ".dedie")
}

// Open prepares the data directory, loads the master key and opens the
// metadata database.
func Open(dataDir string) (*App, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "backups"), 0o700); err != nil {
		return nil, err
	}
	// Tighten permissions on a directory that already existed.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	kr, err := secure.LoadKeyring(dataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	st.FailInterrupted(context.Background())
	return &App{Store: st, keyring: kr, dataDir: dataDir, instance: secure.NewToken(8)}, nil
}

func (a *App) Close() error { return a.Store.Close() }

// Error is a failure with an HTTP-style status the front ends can map
// directly: 400 invalid input, 401 unauthenticated, 403 forbidden,
// 404 not found, 409 conflict with current state.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &Error{Status: 409, Message: fmt.Sprintf(format, args...)}
}

var (
	ErrUnauthenticated = &Error{Status: 401, Message: "sign in required"}
	ErrForbidden       = &Error{Status: 403, Message: "your role does not allow this action"}
	ErrNotFound        = &Error{Status: 404, Message: "not found"}
)

// wrap converts storage errors into application errors.
func wrap(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
