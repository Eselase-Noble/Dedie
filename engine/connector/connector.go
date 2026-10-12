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

// Reusable is implemented by sessions that can be kept open between
// requests instead of being opened anew for each, which on a distant or
// TLS-protected server is most of what a request costs.
type Reusable interface {
	// Alive reports whether the connection still answers.
	Alive(ctx context.Context) bool
	// Reset returns the session to the state it was opened in, undoing
	// whatever statements run on it may have changed: an open
	// transaction, a role, a search path, settings. It reports false when
	// that cannot be done, and the session must then be closed.
	Reset(ctx context.Context) bool
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
	// Accounts lists the server's users and roles with their connection
	// limits, and who is holding connections right now.
	Accounts(ctx context.Context) (*Accounts, error)
	// SaveAccount creates a database user or changes one.
	SaveAccount(ctx context.Context, change AccountChange) error
	// DropAccount removes an account. With handOver, what the account
	// owns in the connected database is first given to the connecting
	// user and its privileges there are revoked, which is what otherwise
	// stops a PostgreSQL role from being dropped.
	DropAccount(ctx context.Context, name, host string, handOver bool) error
	// SetConnectionLimit caps the connections of a user or a database.
	SetConnectionLimit(ctx context.Context, limit LimitChange) error
	// TerminateSessions disconnects the sessions a filter matches and
	// returns how many there were.
	TerminateSessions(ctx context.Context, filter SessionFilter) (int, error)
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

// Unlimited is the connection limit of an account or database that has none.
const Unlimited = -1

// Account is a user or role on a database server.
type Account struct {
	Name string `json:"name"`
	// Host is the client host part of a MySQL account ('user'@'host').
	Host        string `json:"host,omitempty"`
	CanLogin    bool   `json:"can_login"`
	Superuser   bool   `json:"superuser"`
	CreateDB    bool   `json:"create_db"`
	CreateRole  bool   `json:"create_role"`
	Replication bool   `json:"replication"`
	// ConnectionLimit is the most connections the account may hold at
	// once; Unlimited when there is no cap.
	ConnectionLimit int `json:"connection_limit"`
	// Connections is how many it holds now, and of those how many are
	// doing work, waiting for the application, or sitting inside an open
	// transaction.
	Connections       int `json:"connections"`
	Active            int `json:"active"`
	Idle              int `json:"idle"`
	IdleInTransaction int `json:"idle_in_transaction"`
	// OldestIdleSeconds is how long its longest-idle connection has been idle.
	OldestIdleSeconds int64 `json:"oldest_idle_seconds"`
	// MemberOf lists the roles granted to the account.
	MemberOf   []string `json:"member_of"`
	ValidUntil string   `json:"valid_until,omitempty"`
}

// DatabaseLimit is a database's connection cap and current use.
type DatabaseLimit struct {
	Name            string `json:"name"`
	ConnectionLimit int    `json:"connection_limit"`
	Connections     int    `json:"connections"`
}

// ConnectionUsage counts the connections one application holds as one
// user from one address.
type ConnectionUsage struct {
	User        string `json:"user"`
	Application string `json:"application"`
	Client      string `json:"client"`
	Database    string `json:"database"`
	Total       int    `json:"total"`
	Active      int    `json:"active"`
	Idle        int    `json:"idle"`
	// IdleInTransaction connections hold locks and block maintenance
	// while doing nothing; they are the ones worth chasing.
	IdleInTransaction int `json:"idle_in_transaction"`
	// LongestSeconds is the age of the oldest of these connections' current state.
	LongestSeconds int64 `json:"longest_seconds"`
}

// Accounts is the user and connection picture of a server.
type Accounts struct {
	Accounts []Account `json:"accounts"`
	// Roles lists what can be granted to an account.
	Roles          []string          `json:"roles"`
	MaxConnections int               `json:"max_connections"`
	Used           int               `json:"used_connections"`
	Reserved       int               `json:"reserved_connections"`
	Databases      []DatabaseLimit   `json:"databases"`
	Usage          []ConnectionUsage `json:"usage"`
	// DatabaseLimits reports whether this kind of server can cap
	// connections per database.
	DatabaseLimits bool `json:"database_limits"`
}

// Access levels that SaveAccount can grant on a database.
const (
	AccessReadOnly  = "readonly"
	AccessReadWrite = "readwrite"
)

// AccountChange creates an account or changes one. Fields left nil are
// not touched.
type AccountChange struct {
	Create bool   `json:"create"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	// Password is sent once and never stored by Dedie.
	Password        *string  `json:"password"`
	CanLogin        *bool    `json:"can_login"`
	ConnectionLimit *int     `json:"connection_limit"`
	Grant           []string `json:"grant"`
	Revoke          []string `json:"revoke"`
	// Access grants a ready-made level on the connection's database:
	// AccessReadOnly or AccessReadWrite.
	Access string `json:"access"`
}

// Validate checks the parts of a change that are the same for every database.
func (c AccountChange) Validate() error {
	if err := CheckName("user", c.Name); err != nil {
		return err
	}
	if c.Create && c.Password != nil && len(*c.Password) < 8 {
		return errors.New("give the database user a password of at least 8 characters")
	}
	if c.ConnectionLimit != nil && *c.ConnectionLimit < Unlimited {
		return errors.New("the connection limit cannot be negative")
	}
	switch c.Access {
	case "", AccessReadOnly, AccessReadWrite:
	default:
		return fmt.Errorf("unknown access level %q", c.Access)
	}
	for _, r := range append(append([]string{}, c.Grant...), c.Revoke...) {
		if err := CheckName("role", r); err != nil {
			return err
		}
	}
	return nil
}

// LimitChange caps connections. Scope is "account" or "database".
type LimitChange struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	Limit int    `json:"limit"`
}

// SessionFilter selects sessions to disconnect. At least one field must
// be set: an empty filter would disconnect everyone.
type SessionFilter struct {
	User        string `json:"user"`
	Application string `json:"application"`
	Database    string `json:"database"`
	Client      string `json:"client"`
	// State narrows the filter to sessions that are not working:
	// StateIdle, or StateIdleInTransaction. Empty matches any state.
	State string `json:"state"`
	// IdleSeconds, with State, matches only sessions that have been in
	// that state at least this long.
	IdleSeconds int64 `json:"idle_seconds"`
	// ExcludeUsers and ExcludeApplications are never matched, whatever
	// the rest of the filter says.
	ExcludeUsers        []string `json:"exclude_users"`
	ExcludeApplications []string `json:"exclude_applications"`
}

// Session states a filter can select.
const (
	StateIdle              = "idle"
	StateIdleInTransaction = "idle in transaction"
)

func (f SessionFilter) Empty() bool {
	return f.User == "" && f.Application == "" && f.Database == "" && f.Client == "" && f.State == ""
}

// Validate rejects a filter that names an unknown state.
func (f SessionFilter) Validate() error {
	switch f.State {
	case "", StateIdle, StateIdleInTransaction:
	default:
		return fmt.Errorf("unknown session state %q", f.State)
	}
	if f.IdleSeconds < 0 || (f.IdleSeconds > 0 && f.State == "") {
		return errors.New("an idle time needs a state to apply to")
	}
	return nil
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
