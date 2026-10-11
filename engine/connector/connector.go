// Package connector defines the contract every database driver implements.
// The query browser, the administration screens and the migration pipelines
// are written against these interfaces, so supporting a new database means
// adding one sub-package and registering it here.
package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Config is everything needed to reach a database. The password is held in
// memory only for the lifetime of a call; at rest it is envelope-encrypted.
type Config struct {
	Host        string
	Port        int
	Database    string
	Username    string
	Password    string
	SSLMode     string
	SSLRootCert string // PEM; optional private CA for verify-ca / verify-full
}

// Driver opens sessions for one kind of database.
type Driver interface {
	Kind() string
	DefaultPort() int
	Open(ctx context.Context, cfg Config) (Session, error)
}

// Session is one live connection.
type Session interface {
	Info(ctx context.Context) (ServerInfo, error)
	Catalog(ctx context.Context) (*Catalog, error)
	Query(ctx context.Context, sql string, opts QueryOptions) ([]ResultSet, error)
	Overview(ctx context.Context) (*Overview, error)
	// CancelBackend cancels the running statement of another backend, or
	// terminates the backend entirely when terminate is set.
	CancelBackend(ctx context.Context, pid int64, terminate bool) error
	Close(ctx context.Context) error
}

type ServerInfo struct {
	Kind       string `json:"kind"`
	Version    string `json:"version"`
	VersionNum int    `json:"version_num"`
	Database   string `json:"database"`
	User       string `json:"user"`
	Encrypted  bool   `json:"encrypted"`
	Superuser  bool   `json:"superuser"`
}

type QueryOptions struct {
	// ReadOnly runs a single statement inside a read-only transaction.
	ReadOnly bool
	MaxRows  int
	// TimeoutSeconds bounds statement execution on the server.
	TimeoutSeconds int
}

type ResultSet struct {
	Columns   []Column    `json:"columns"`
	Rows      [][]*string `json:"rows"`
	Truncated bool        `json:"truncated"`
	Command   string      `json:"command"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Catalog is the object tree shown in the query browser.
type Catalog struct {
	Schemas []Schema `json:"schemas"`
}

type Schema struct {
	Name      string   `json:"name"`
	Tables    []Table  `json:"tables"`
	Views     []Table  `json:"views"`
	Functions []string `json:"functions"`
	Sequences []string `json:"sequences"`
}

type Table struct {
	Name    string        `json:"name"`
	EstRows int64         `json:"est_rows"`
	Size    int64         `json:"size_bytes"`
	Columns []TableColumn `json:"columns"`
	Indexes []string      `json:"indexes"`
}

type TableColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	PrimaryKey bool   `json:"primary_key"`
	Default    string `json:"default,omitempty"`
}

// Overview is the server health snapshot used by the administration screen.
type Overview struct {
	Databases        []DatabaseSize    `json:"databases"`
	Activity         []Backend         `json:"activity"`
	ReplicationSlots []ReplicationSlot `json:"replication_slots"`
	Settings         map[string]string `json:"settings"`
}

type DatabaseSize struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

type Backend struct {
	PID         int64  `json:"pid"`
	User        string `json:"user"`
	Database    string `json:"database"`
	Application string `json:"application"`
	Client      string `json:"client"`
	State       string `json:"state"`
	WaitEvent   string `json:"wait_event"`
	Seconds     int64  `json:"seconds"`
	Query       string `json:"query"`
}

type ReplicationSlot struct {
	Name          string `json:"name"`
	Plugin        string `json:"plugin"`
	Database      string `json:"database"`
	Active        bool   `json:"active"`
	RetainedBytes int64  `json:"retained_bytes"`
}

var (
	mu      sync.RWMutex
	drivers = map[string]Driver{}
)

// Register makes a driver available under its kind. Drivers call this from init.
func Register(d Driver) {
	mu.Lock()
	defer mu.Unlock()
	drivers[d.Kind()] = d
}

// Get returns the driver for a database kind.
func Get(kind string) (Driver, error) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported database kind %q", kind)
	}
	return d, nil
}

// Kinds lists the registered database kinds.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(drivers))
	for k := range drivers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
