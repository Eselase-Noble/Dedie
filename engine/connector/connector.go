// Package connector defines the contract every database driver implements.
// The query browser, the administration screens and the migration pipelines
// are written against these interfaces, so supporting a new database means
// adding one sub-package and registering it here.
package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	// CreateDatabase, CreateSchema and CreateTable create objects on the
	// server. Whether the account may do so is the server's decision; its
	// refusal comes back as the error.
	CreateDatabase(ctx context.Context, name string) error
	CreateSchema(ctx context.Context, name string) error
	CreateTable(ctx context.Context, def TableDef) error
	// ServerObjects lists the server's databases and roles.
	ServerObjects(ctx context.Context) (*ServerObjects, error)
	// SetOwner hands a database, schema or table to another role.
	SetOwner(ctx context.Context, change OwnerChange) error
	// ChangeRow inserts, updates or deletes exactly one row. Values are
	// bound as parameters, never spliced into the statement.
	ChangeRow(ctx context.Context, change RowChange) error
	Close(ctx context.Context) error
}

// ServerObjects is what exists on a server beyond the connected database.
type ServerObjects struct {
	Databases []Database `json:"databases"`
	// Roles lists the roles an object can be given to. It is empty for
	// databases that have no notion of object ownership.
	Roles []string `json:"roles"`
	// Ownership reports whether objects have owners on this kind of server.
	Ownership bool `json:"ownership"`
}

type Database struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
}

// Object kinds whose owner can be changed.
const (
	OwnDatabase = "database"
	OwnSchema   = "schema"
	OwnTable    = "table"
)

// OwnerChange names an object and the role that should own it.
type OwnerChange struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Owner  string `json:"owner"`
}

// Row actions.
const (
	RowInsert = "insert"
	RowUpdate = "update"
	RowDelete = "delete"
)

// RowChange is one edit made in the data browser. Values are in the text
// form the database prints; nil means NULL.
type RowChange struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Action string `json:"action"`
	// Key identifies the row to update or delete: its primary key columns
	// and their current values.
	Key map[string]*string `json:"key"`
	// Values holds the columns to set. A column left out of an insert
	// takes its default.
	Values map[string]*string `json:"values"`
}

// Validate checks that a change says enough to be applied safely.
func (c RowChange) Validate() error {
	if err := CheckName("table", c.Table); err != nil {
		return err
	}
	switch c.Action {
	case RowInsert:
	case RowUpdate:
		if len(c.Values) == 0 {
			return errors.New("nothing to change")
		}
		fallthrough
	case RowDelete:
		if len(c.Key) == 0 {
			return errors.New("the row cannot be identified: the table has no primary key")
		}
		for col, v := range c.Key {
			if v == nil {
				return fmt.Errorf("the key column %s is NULL, so the row cannot be identified", col)
			}
		}
	default:
		return fmt.Errorf("unknown row action %q", c.Action)
	}
	for col := range c.Values {
		if err := CheckName("column", col); err != nil {
			return err
		}
	}
	return nil
}

// Columns returns the names of a column map in a stable order, so the
// statement built from it is the same every time.
func Columns(m map[string]*string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrRowCount is returned when an update or delete would not touch exactly
// one row; the change is rolled back.
func ErrRowCount(n int64) error {
	if n == 0 {
		return errors.New("the row no longer exists or has changed; reload the table. Nothing was changed")
	}
	return fmt.Errorf("the key matches %d rows instead of one; nothing was changed", n)
}

// TableDef describes a table to create.
type TableDef struct {
	Schema  string      `json:"schema"`
	Name    string      `json:"name"`
	Columns []ColumnDef `json:"columns"`
}

type ColumnDef struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	PrimaryKey bool   `json:"primary_key"`
	// Default is a number, a keyword such as CURRENT_TIMESTAMP, or text
	// that is stored as a string literal. Empty means no default.
	Default string `json:"default"`
}

var (
	typeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*(\([0-9]+(, ?[0-9]+)?\))?( [A-Za-z ]+)?(\[\])?$`)
	// rawDefaultRe matches defaults that are written as they are; anything
	// else becomes a quoted string.
	rawDefaultRe = regexp.MustCompile(`(?i)^(-?[0-9]+(\.[0-9]+)?|true|false|null|current_timestamp|current_date|now\(\)|gen_random_uuid\(\)|uuid\(\))$`)
)

// CheckName validates the name of an object to create. Quoting makes any
// name safe; this only rejects what no database accepts.
func CheckName(kind, name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("enter a %s name", kind)
	case len(name) > 63:
		return fmt.Errorf("the %s name is longer than 63 characters", kind)
	case strings.ContainsAny(name, "\x00\n\r"):
		return fmt.Errorf("the %s name contains characters that are not allowed", kind)
	}
	return nil
}

// Validate checks a table definition before any SQL is built from it.
// Column types are the one part that cannot be quoted, so they must look
// like a type name with an optional size.
func (d TableDef) Validate() error {
	if err := CheckName("table", d.Name); err != nil {
		return err
	}
	if len(d.Columns) == 0 {
		return errors.New("a table needs at least one column")
	}
	seen := map[string]bool{}
	for _, c := range d.Columns {
		if err := CheckName("column", c.Name); err != nil {
			return err
		}
		if seen[strings.ToLower(c.Name)] {
			return fmt.Errorf("column %s is listed twice", c.Name)
		}
		seen[strings.ToLower(c.Name)] = true
		if !typeRe.MatchString(strings.TrimSpace(c.Type)) {
			return fmt.Errorf("column %s: %q is not a valid type", c.Name, c.Type)
		}
	}
	return nil
}

// DefaultSQL renders a column default: numbers and known keywords as
// written, anything else as a string literal.
func DefaultSQL(def string) string {
	def = strings.TrimSpace(def)
	if rawDefaultRe.MatchString(def) {
		return def
	}
	return "'" + strings.ReplaceAll(def, "'", "''") + "'"
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
	Owner     string   `json:"owner,omitempty"`
	Tables    []Table  `json:"tables"`
	Views     []Table  `json:"views"`
	Functions []string `json:"functions"`
	Sequences []string `json:"sequences"`
}

type Table struct {
	Name    string        `json:"name"`
	Owner   string        `json:"owner,omitempty"`
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
