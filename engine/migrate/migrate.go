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
type Config struct {
	// Schemas limits the migration to these schemas; empty means all.
	// MySQL pipelines migrate the connection's database and ignore this.
	Schemas []string `json:"schemas"`
	// ExcludeTables lists "schema.table" names to leave out.
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
	// CutoverTimeoutSeconds bounds how long cutover waits for the stream to
	// drain before giving up and resuming normal streaming.
	CutoverTimeoutSeconds int `json:"cutover_timeout_seconds"`
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
