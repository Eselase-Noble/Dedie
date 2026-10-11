// Package mongopg migrates a MongoDB database into PostgreSQL while the
// source stays in use.
//
// Each collection becomes a table of two columns: _id, the document's key
// as text, and doc, the whole document as jsonb. Nothing in a document is
// lost or reinterpreted, and its fields are reachable from SQL with the
// jsonb operators (doc->>'email', doc->'address'->>'city'). Fields can
// also be lifted into typed columns of their own, beside the document.
//
// The copy is followed by MongoDB's change stream, which needs the source
// to be a replica set. The stream is opened at a point taken before the
// copy starts, and every change is applied as "store this document" or
// "delete this key", so replaying a change that the copy already included
// is harmless. The stream's position is committed on the target in the
// same transaction as the changes it covers.
//
// An offline job copies the collections once and opens no change stream, so
// a standalone server will do. MongoDB offers no snapshot that spans a long
// copy, which makes it the operator's part to keep the source still: the
// final validation fails the job if the collections changed meanwhile.
package mongopg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

func init() { migrate.Register(mongodb.Kind, postgres.Kind, pipeline{}) }

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

func (pipeline) Preview(ctx context.Context, src, dst connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	return Preview(ctx, src, dst, cfg)
}

// State is the job's durable progress. Field names match the other
// pipelines where the meaning is the same, so the interface shows them
// alike.
type State struct {
	SchemaDone   bool `json:"schema_done"`
	SnapshotDone bool `json:"snapshot_done"`
	// StartT and StartI are the cluster time taken before the copy began;
	// the stream starts there the first time.
	StartT uint32 `json:"start_t"`
	StartI uint32 `json:"start_i"`
	// AppliedLSN is the cluster time of the last change applied, for display.
	AppliedLSN  string `json:"applied_lsn"`
	LagSeconds  int64  `json:"lag_seconds"`
	LagBytes    int64  `json:"lag_bytes"`
	TxApplied   int64  `json:"tx_applied"`
	RowsApplied int64  `json:"rows_applied"`
	Anomalies   int64  `json:"anomalies"`
	// Unconverted counts field values a lifted column could not hold and
	// stored as NULL; the document keeps the original.
	Unconverted    int64     `json:"unconverted"`
	LastCommitTime time.Time `json:"last_commit_time"`
	SourceVersion  string    `json:"source_version"`
	TargetVersion  string    `json:"target_version"`

	// What the job does about tables that were on the target before it
	// started. It is decided once, in the schema phase, from the policy
	// chosen for the job: later the job's own tables exist as well, and
	// nothing on the target tells the two apart.
	//
	// SkippedTables are the collections left out of the migration because
	// the target's tables of their names are kept.
	SkippedTables []string `json:"skipped_tables,omitempty"`
	// DropFirst are the statements that remove what the job replaces.
	DropFirst []string `json:"drop_first,omitempty"`
	// Tables are the collections whose tables the schema phase prepared.
	// The copy empties and fills those and no others: a collection that
	// appears later arrives through the stream.
	Tables []string `json:"tables,omitempty"`
}

// targetSchema is where the collections' tables are created.
const targetSchema = "public"

// metaSchema holds the checkpoint table on the target while a job runs.
const metaSchema = "dedie"

const checkpointDDL = `CREATE SCHEMA IF NOT EXISTS ` + metaSchema + `;
CREATE TABLE IF NOT EXISTS ` + metaSchema + `.mongo_checkpoint (
	job_id text PRIMARY KEY, resume_token bytea NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

// checkpointMark records that a job has started preparing the target,
// before it creates anything there, so that a retry can tell its own
// leftovers from somebody else's tables. The empty token is no position: a
// position left by an earlier run of the job would send the stream to the
// wrong place.
const checkpointMark = `INSERT INTO ` + metaSchema + `.mongo_checkpoint (job_id, resume_token) VALUES ($1, '')
	ON CONFLICT (job_id) DO UPDATE SET resume_token = '', updated_at = now()`

const checkpointUpsert = `INSERT INTO ` + metaSchema + `.mongo_checkpoint (job_id, resume_token) VALUES ($1, $2)
	ON CONFLICT (job_id) DO UPDATE SET resume_token = excluded.resume_token, updated_at = now()`

func qname(table string) string { return pgx.Identifier{targetSchema, table}.Sanitize() }
