// Package migrate defines what every migration pipeline has in common: the
// operator's options, the phases a job moves through, how progress is
// reported, and a registry that maps a (source kind, target kind) pair to the
// pipeline that handles it. Each pipeline lives in its own sub-package.
package migrate

import (
	"context"
	"encoding/json"
	"fmt"

	"dedie/engine/connector"
)

// Phases, in the order a job moves through them.
const (
	PhaseSchema    = "schema"
	PhaseSnapshot  = "snapshot"
	PhaseIndexing  = "indexing"
	PhaseStreaming = "streaming"
	PhaseCutover   = "cutover"
	PhaseDone      = "done"
)

// Outcome says why a pipeline returned without an error.
type Outcome string

const (
	Completed Outcome = "completed"
	Paused    Outcome = "paused"
	Aborted   Outcome = "aborted"
)

// Config is the operator's choices for one migration.
//
// What gets migrated is decided at one of three levels:
//
//	database  nothing listed: every schema and table of the connection's database
//	schema    Schemas listed: everything in those schemas
//	table     Tables listed: only those tables, with the types, sequences and
//	          functions they may depend on, their indexes, constraints and
//	          triggers; views and other standalone objects are left out
type Config struct {
	// Mode is ModeMigrate (the default) for a move that ends with a
	// cutover, ModeSync to keep the target following the source for as
	// long as the job runs, or ModeOffline for a one-time copy. Migrate
	// and sync do the same work; a sync is simply not expected to end,
	// and never fences its source. An offline migration copies from one
	// consistent snapshot, validates and finishes by itself: it follows
	// no changes, so the source needs no replication set up, and writes
	// made to the source after the copy starts are not carried over.
	Mode string `json:"mode"`
	// Schemas limits the migration to these schemas; empty means all. In
	// MySQL a schema is the database itself, so its pipeline ignores this.
	Schemas []string `json:"schemas"`
	// Tables limits the migration to these tables, named "schema.table"
	// for PostgreSQL and "table" for MySQL. Empty means all tables.
	Tables []string `json:"tables"`
	// ExcludeTables lists tables to leave out, named the same way.
	ExcludeTables []string `json:"exclude_tables"`
	// SnapshotWorkers is the number of tables copied in parallel.
	SnapshotWorkers int `json:"snapshot_workers"`
	// FenceSource makes the source database read-only at cutover, so no
	// write can slip in after the final sync.
	FenceSource bool `json:"fence_source"`
	// Validation is "checksum" (default), "count" or "none".
	Validation string `json:"validation"`
	// KeepSlot leaves the replication slot and publication on a PostgreSQL
	// source after completion.
	KeepSlot bool `json:"keep_slot"`
	// Columns lifts fields of documents into typed columns when a
	// document store is migrated into tables, keyed by collection. The
	// whole document is still stored; these are for querying and indexing.
	Columns map[string][]ColumnSpec `json:"columns,omitempty"`
	// OnExisting says what to do when the target already holds an object
	// with the name of one being migrated: ExistingFail (the default),
	// ExistingSkip or ExistingReplace. See those constants.
	OnExisting string `json:"on_existing,omitempty"`
	// Code says what a migration between two kinds of database does with
	// stored functions, procedures and triggers: CodeConverted (the
	// default), CodeReview or CodeOff. See those constants. A migration
	// between two databases of one kind copies them as they are and
	// ignores this.
	Code string `json:"code,omitempty"`
	// CutoverTimeoutSeconds bounds how long cutover waits for the stream to
	// drain before giving up and resuming normal streaming.
	CutoverTimeoutSeconds int `json:"cutover_timeout_seconds"`
}

// ColumnSpec lifts one field of a document into a column.
type ColumnSpec struct {
	// Field is the path of the field, with dots for nesting: "address.city".
	Field string `json:"field"`
	// Name is the column's name; empty derives it from the field.
	Name string `json:"name,omitempty"`
	// Type is the column's type on the target.
	Type string `json:"type"`
}

// Migration modes.
const (
	ModeMigrate = "migrate"
	ModeSync    = "sync"
	ModeOffline = "offline"
)

// What a migration does about objects that already exist on the target.
// The choice is the database administrator's: preflight lists the objects
// in Report.Existing, and nothing is skipped or dropped unless one of the
// two permissive policies was chosen for the job.
const (
	// ExistingFail refuses to start. Preflight fails and names the objects.
	ExistingFail = "fail"
	// ExistingSkip leaves every existing object exactly as it is. An
	// existing table is left out of the migration altogether: no rows
	// are copied into it and changes to its source are not followed.
	// Other existing objects (views, routines, sequences and so on) are
	// kept and the source's version is not created.
	ExistingSkip = "skip"
	// ExistingReplace drops each existing object and creates it again
	// from the source. For a table that destroys the data it held on
	// the target, along with whatever depends on it there.
	ExistingReplace = "replace"
)

// What a migration between two kinds of database does with stored
// functions, procedures and triggers. The converter says of each routine
// whether every construct in it has a rewrite that keeps its behaviour
// ("converted"), whether some rewrite may behave differently ("review"),
// or whether it has no rewrite at all; a routine of the last sort is never
// created.
const (
	// CodeConverted creates the routines the converter vouches for. Those
	// that need a person's review are not created: their converted draft
	// and the notes on what to check are handed to the administrator.
	CodeConverted = "converted"
	// CodeReview also creates the routines that need review, and reports
	// them as such.
	CodeReview = "review"
	// CodeOff converts nothing; all stored code is left for the
	// administrator, with its definition on the source.
	CodeOff = "off"
)

// CodeMode returns what the job does with stored code, with the default
// filled in.
func (c Config) CodeMode() string {
	switch c.Code {
	case CodeReview, CodeOff:
		return c.Code
	}
	return CodeConverted
}

// Existing returns the policy for objects already on the target, with
// the default filled in.
func (c Config) Existing() string {
	switch c.OnExisting {
	case ExistingSkip, ExistingReplace:
		return c.OnExisting
	}
	return ExistingFail
}

// ExistingObject is something on the target that has the name of an
// object the migration would create.
type ExistingObject struct {
	// Kind is "table", "view", "function", "sequence", "trigger" and so on.
	Kind string `json:"kind"`
	// Name is how the target knows it, qualified where that applies.
	Name string `json:"name"`
}

// Offline reports whether the job is a one-time copy that follows no changes.
func (c Config) Offline() bool { return c.Mode == ModeOffline }

// Scope names the level the migration runs at: "table", "schema" or "database".
func (c Config) Scope() string {
	switch {
	case len(c.Tables) > 0:
		return "table"
	case len(c.Schemas) > 0:
		return "schema"
	}
	return "database"
}

// Defaults fills in unset options.
func (c *Config) Defaults() {
	if c.SnapshotWorkers <= 0 {
		c.SnapshotWorkers = 4
	}
	if c.SnapshotWorkers > 16 {
		c.SnapshotWorkers = 16
	}
	if c.Validation == "" {
		c.Validation = "checksum"
	}
	if c.CutoverTimeoutSeconds <= 0 {
		c.CutoverTimeoutSeconds = 300
	}
}

// Reporter is how a pipeline talks to whatever is running it. The engine
// persists these calls; tests can record them.
type Reporter interface {
	Event(level, message string)
	SetPhase(phase string)
	// SaveState stores the pipeline's durable progress; it must marshal to JSON.
	SaveState(state any)
	ResetTables(tables []TableProgress)
	UpdateTable(schema, table string, fields map[string]any)
	// Desired returns the operator's latest instruction:
	// "run", "pause", "cutover" or "abort".
	Desired() string
	SetDesired(string)
}

type TableProgress struct {
	Schema  string
	Table   string
	EstRows int64
}

// Check is one preflight finding. Status is "ok", "warn" or "fail"; any
// "fail" blocks the migration from starting.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
	Tables int     `json:"tables"`
	// Existing lists the objects already on the target that the
	// migration would create, whatever the policy; it is what the
	// operator is shown when asked to choose one.
	Existing []ExistingObject `json:"existing,omitempty"`
}

func (r *Report) Add(name, status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

// Finish sets OK from the recorded checks.
func (r *Report) Finish() {
	r.OK = true
	for _, c := range r.Checks {
		if c.Status == "fail" {
			r.OK = false
		}
	}
}

// Pipeline migrates between one pair of database kinds.
type Pipeline interface {
	// Preflight inspects both databases without changing either.
	Preflight(ctx context.Context, src, dst connector.Config, cfg Config) (*Report, error)
	// Run drives a job from its saved state (empty for a new job) until it
	// completes or the operator pauses or aborts it.
	Run(ctx context.Context, jobID string, cfg Config, state json.RawMessage, src, dst connector.Config, rep Reporter) (Outcome, error)
}

// Statement is one statement a migration would run on the target.
type Statement struct {
	// Phase is when it runs: PhaseSchema, PhaseIndexing or PhaseCutover.
	Phase string `json:"phase"`
	// Kind is what it creates: "schema", "table", "index", "foreign key"...
	Kind string `json:"kind"`
	Name string `json:"name"`
	SQL  string `json:"sql"`
	// End is what closes the statement in a script when a semicolon does
	// not: "/" for an Oracle program unit, whose own text ends in one,
	// and "//" for a MySQL routine, whose body has semicolons inside and
	// needs the client told of another delimiter.
	End string `json:"end,omitempty"`
}

// Previewer is implemented by pipelines that can show the statements they
// would run on the target, in order, without running any of them. It is
// how an administrator reads a translated schema before agreeing to it.
// Like preflight, a preview changes neither database.
type Previewer interface {
	Preview(ctx context.Context, src, dst connector.Config, cfg Config) ([]Statement, error)
}

// Leftover is an object of the source that a migration does not carry
// over, with its definition as the source has it.
type Leftover struct {
	// Kind is what it is: "view", "function", "procedure", "trigger",
	// "package" and so on.
	Kind string `json:"kind"`
	// Name is how the source knows it, qualified where that applies.
	Name string `json:"name"`
	// Reason says why it is not migrated.
	Reason string `json:"reason"`
	// Source is the statement that defines it, in the source's dialect.
	// It is empty when the source would not show the definition.
	Source string `json:"source"`
	// End is what closes the statement in a script when a semicolon does
	// not: "/" for an Oracle program unit, whose own text ends in one.
	End string `json:"end,omitempty"`

	// The rest is filled in for stored code the migration tried to
	// convert.

	// Status is how far the conversion got: "review" for a routine that
	// was converted but not created because a person has to check it,
	// "unconverted" for one that has no conversion, "not created" for
	// one the target refused, and "created, needs review" for one that
	// is on the target and still wants checking. It is empty for views
	// and for code no conversion was tried on.
	Status string `json:"status,omitempty"`
	// Notes say, naming the line of the source, what to check or why
	// the routine could not be converted.
	Notes []string `json:"notes,omitempty"`
	// Draft is the converted routine in the target's dialect, as a
	// script, where there is one: the place to start from.
	Draft string `json:"draft,omitempty"`
}

// LeftoverLister is implemented by pipelines that leave some of the
// source behind: stored code, which no pipeline between two different
// databases translates, and the views it could not translate with the
// same meaning. Rather than only saying so, such a pipeline hands the
// administrator the definitions, so that converting them by hand can
// start from one script instead of from a search through the source.
//
// state is the job's saved state, empty for a job that has not run; a
// pipeline reads from it which views it created and had to drop again.
// Like preflight, listing changes neither database.
type LeftoverLister interface {
	Leftovers(ctx context.Context, src, dst connector.Config, cfg Config, state json.RawMessage) ([]Leftover, error)
}

var pipelines = map[string]Pipeline{}

// Kind names the pipeline for a source and target database kind.
func Kind(sourceKind, targetKind string) string { return sourceKind + "-" + targetKind }

// Register makes a pipeline available; pipelines call it from init.
func Register(sourceKind, targetKind string, p Pipeline) {
	pipelines[Kind(sourceKind, targetKind)] = p
}

// Lookup returns the pipeline registered under kind.
func Lookup(kind string) (Pipeline, error) {
	p, ok := pipelines[kind]
	if !ok {
		return nil, fmt.Errorf("migrating %s is not supported yet", kind)
	}
	return p, nil
}
