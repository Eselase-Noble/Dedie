// Package oraora migrates an Oracle database into another Oracle
// database: onto a newer release, or onto another server, either as a
// one-time copy or while the source stays in use.
//
// Both sides speak the same dialect, so nothing is translated. Every
// object is created from the definition the source itself writes with
// DBMS_METADATA, stripped of what belongs to the old server rather than to
// the object (tablespaces, storage and other segment attributes), so that
// the target's own defaults apply. Each schema's objects go to the schema
// of the same name; a single schema may instead be received by the target
// account's own schema, in which case the definitions are rewritten for it.
//
// What is created when follows from how the data arrives. Tables and
// sequences come first. Stored code, views and synonyms follow the copy,
// so that a dependency that does not compile cannot hold the data up, and
// indexes and constraints are built over the finished tables. Foreign keys
// wait until the data has been validated, because rows are applied table
// by table while changes are followed and not in the order the source
// wrote them; triggers come last of all, so that they never fire for a
// row that was copied or replicated.
//
// Tables are copied from a flashback query, so all of them are read as of
// one system change number (SCN) without holding a lock on the source.
// Changes after that point are found with LogMiner. The redo stream is
// used only to learn which rows a committed transaction touched; each row
// is then read back from the source as of that commit and stored whole.
// The position in the redo stream is committed on the target together
// with the rows it covers.
//
// Everything that concerns the source alone (its catalog, reading values,
// consistent reads, the redo stream) lives in package orasrc, which the
// other pipelines out of Oracle share. What is here is the Oracle target,
// and the reading of definitions, which only this pipeline needs.
package oraora

import (
	"context"
	"encoding/json"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

func init() { migrate.Register(oracle.Kind, oracle.Kind, pipeline{}) }

// pipeline adapts this package to the engine's pipeline registry.
type pipeline struct{}

func (pipeline) Preflight(ctx context.Context, src, dst connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	return Preflight(ctx, src, dst, cfg)
}

func (pipeline) Run(ctx context.Context, jobID string, cfg migrate.Config, state json.RawMessage, src, dst connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	var st State
	if len(state) > 0 {
		_ = json.Unmarshal(state, &st)
	}
	return Run(ctx, jobID, cfg, st, src, dst, rep)
}

// State is the job's durable progress. Field names match the other
// pipelines where the meaning is the same, so the interface shows them
// alike.
type State struct {
	SchemaDone   bool `json:"schema_done"`
	SnapshotDone bool `json:"snapshot_done"`
	IndexingDone bool `json:"indexing_done"`
	// SnapshotSCN is the moment the copy reads the source at.
	SnapshotSCN int64 `json:"snapshot_scn"`
	// MineFrom is where the redo stream is first read from. It lies at or
	// before the snapshot, early enough to include every transaction that
	// was still open when the snapshot was taken.
	MineFrom int64 `json:"mine_from"`
	// LogGroups lists the tables this job switched primary key logging on
	// for, so that it can switch it off again when it is done.
	LogGroups []string `json:"log_groups,omitempty"`

	// ExistingDecided is set once the job has looked at what the target
	// already held and acted on the operator's choice. It is looked at
	// once only: from then on the job's own objects exist there too, and
	// nothing could tell them from what was there before.
	ExistingDecided bool `json:"existing_decided,omitempty"`
	// SkippedTables are the tables that were already on the target and
	// are kept as they were, by their name there. They take no part in
	// the migration.
	SkippedTables []string `json:"skipped_tables,omitempty"`
	// SkippedSources are the same tables by their name on the source,
	// which is how they are left out of the catalog.
	SkippedSources []string `json:"skipped_sources,omitempty"`
	// SkippedObjects are the other objects that were already on the
	// target and are kept: each is written "kind SCHEMA.NAME".
	SkippedObjects []string `json:"skipped_objects,omitempty"`

	ConsistentLSN  string    `json:"consistent_lsn"`
	AppliedLSN     string    `json:"applied_lsn"`
	LagSeconds     int64     `json:"lag_seconds"`
	LagBytes       int64     `json:"lag_bytes"`
	TxApplied      int64     `json:"tx_applied"`
	RowsApplied    int64     `json:"rows_applied"`
	Anomalies      int64     `json:"anomalies"`
	LastCommitTime time.Time `json:"last_commit_time"`
	SourceVersion  string    `json:"source_version"`
	TargetVersion  string    `json:"target_version"`
}

// checkpointTable holds, on the target, the position each running job
// has reached in the source's redo stream. It lives in the schema that
// receives the data, is created by the job and dropped when the last job
// using it has finished.
const checkpointTable = "DEDIE_CHECKPOINT"
