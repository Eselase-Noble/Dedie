// Package pgpg migrates one PostgreSQL database into another, typically a
// newer major version, while the source stays online.
//
// The pipeline has five phases:
//
//	schema     recreate types, sequences, functions and tables on the target
//	snapshot   copy every table from one consistent snapshot of the source
//	indexing   build secondary indexes, foreign keys and views
//	streaming  apply changes from the source's logical replication stream
//	cutover    drain the stream, sync sequences, validate and finalize
//
// The replication slot is created together with the snapshot, so the stream
// begins exactly where the snapshot ends: no change is lost or applied twice.
// While streaming, every source transaction is applied as one target
// transaction that also records its position in a checkpoint table, which
// makes a restart after a crash exactly-once.
package pgpg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

func init() { migrate.Register(postgres.Kind, postgres.Kind, pipeline{}) }

// pipeline adapts this package to the engine's pipeline registry.
type pipeline struct{}

func (pipeline) Preflight(ctx context.Context, src, dst connector.Config, cfg Config) (*Report, error) {
	return Preflight(ctx, src, dst, cfg)
}

func (pipeline) Run(ctx context.Context, jobID string, cfg Config, state json.RawMessage, src, dst connector.Config, rep Reporter) (Outcome, error) {
	var st State
	if len(state) > 0 {
		_ = json.Unmarshal(state, &st)
	}
	return Run(ctx, jobID, cfg, st, src, dst, rep)
}

// The shared vocabulary of all pipelines, re-exported for brevity.
const (
	PhaseSchema    = migrate.PhaseSchema
	PhaseSnapshot  = migrate.PhaseSnapshot
	PhaseIndexing  = migrate.PhaseIndexing
	PhaseStreaming = migrate.PhaseStreaming
	PhaseCutover   = migrate.PhaseCutover
	PhaseDone      = migrate.PhaseDone

	Completed = migrate.Completed
	Paused    = migrate.Paused
	Aborted   = migrate.Aborted
)

type (
	Config        = migrate.Config
	Outcome       = migrate.Outcome
	Reporter      = migrate.Reporter
	TableProgress = migrate.TableProgress
	Check         = migrate.Check
	Report        = migrate.Report
)

// State is the durable progress of a job. It is saved after every phase and
// roughly once a second while streaming.
type State struct {
	Slot         string `json:"slot"`
	Publication  string `json:"publication"`
	SchemaDone   bool   `json:"schema_done"`
	SnapshotDone bool   `json:"snapshot_done"`
	IndexingDone bool   `json:"indexing_done"`
	Fenced       bool   `json:"fenced"`
	// ReplicaRole records that the target lets the apply session run with
	// session_replication_role = replica, which keeps foreign-key actions
	// and triggers from firing a second time for replicated rows.
	ReplicaRole    bool      `json:"replica_role"`
	ConsistentLSN  string    `json:"consistent_lsn"`
	AppliedLSN     string    `json:"applied_lsn"`
	SourceLSN      string    `json:"source_lsn"`
	LagBytes       int64     `json:"lag_bytes"`
	TxApplied      int64     `json:"tx_applied"`
	RowsApplied    int64     `json:"rows_applied"`
	Anomalies      int64     `json:"anomalies"`
	LastCommitTime time.Time `json:"last_commit_time"`
	SourceVersion  string    `json:"source_version"`
	TargetVersion  string    `json:"target_version"`
}

// metaSchema holds the checkpoint table on the target while a job runs.
const metaSchema = "dedie"

func qname(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }

func qident(name string) string { return pgx.Identifier{name}.Sanitize() }
