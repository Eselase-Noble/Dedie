// Package orapg migrates an Oracle database into PostgreSQL, either as a
// one-time copy or while the source stays in use.
//
// The schema is translated rather than copied: every Oracle type maps to
// a PostgreSQL type that can hold all of its values, and whatever cannot
// be carried over (PL/SQL, expression defaults PostgreSQL does not know)
// is reported instead of guessed. Views are translated where every
// construct they use has an equivalent that means the same, created once
// the data is in, and kept only if they then return what the source's
// views return; see package viewsql. Each Oracle schema becomes a
// PostgreSQL schema of the same name, and names that Oracle stores in
// upper case are written in lower case, which is what unquoted names
// mean in each database.
//
// Tables are copied from a flashback query, so all of them are read as of
// one system change number (SCN) without holding a lock on the source.
// Changes after that point are found with LogMiner. The redo stream is
// used only to learn which rows a committed transaction touched; each row
// is then read back from the source as of that commit and stored whole.
// That keeps large objects and every data type on the same code path as
// the copy, and makes replaying a change harmless. The position in the
// redo stream is committed on the target together with the rows it covers.
//
// Everything that concerns the source alone (its catalog, reading values,
// consistent reads, the redo stream) lives in package orasrc, which the
// other pipelines out of Oracle share. What is here is the PostgreSQL side.
package orapg

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

func init() { migrate.Register(oracle.Kind, postgres.Kind, pipeline{}) }

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

func (pipeline) Leftovers(ctx context.Context, src, dst connector.Config, cfg migrate.Config, state json.RawMessage) ([]migrate.Leftover, error) {
	var st State
	if len(state) > 0 {
		_ = json.Unmarshal(state, &st)
	}
	return Leftovers(ctx, src, dst, cfg, st)
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

	// What the job does about objects that were on the target before it
	// started. It is decided once, in the schema phase, from the policy
	// chosen for the job: later the job's own objects exist as well, and
	// nothing on the target tells the two apart.
	//
	// SkippedTables are the tables left out of the migration because the
	// target's are kept, named as the job's table selection names them.
	SkippedTables []string `json:"skipped_tables,omitempty"`
	// KeptObjects are the other objects that are kept: the plan neither
	// creates nor alters them.
	KeptObjects []string `json:"kept_objects,omitempty"`
	// DropFirst are the statements that remove what the job replaces.
	DropFirst []string `json:"drop_first,omitempty"`

	// Views says what became of each view the job translated and
	// created: kept and verified, kept without verification, or dropped
	// again. See viewsql.Checker.
	Views []viewsql.Result `json:"views,omitempty"`
	// Code says the same of each converted function, procedure and
	// trigger the job set out to create; see storedcode.Target.
	Code []storedcode.Outcome `json:"code,omitempty"`
	// Objects counts, once the job has finished, what it did with views
	// and stored code.
	Objects *storedcode.Summary `json:"objects,omitempty"`
}

// metaSchema holds the checkpoint table on the target while a job runs.
const metaSchema = "dedie"

const checkpointDDL = `CREATE SCHEMA IF NOT EXISTS ` + metaSchema + `;
CREATE TABLE IF NOT EXISTS ` + metaSchema + `.oracle_checkpoint (
	job_id text PRIMARY KEY, scn bigint NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

const checkpointUpsert = `INSERT INTO ` + metaSchema + `.oracle_checkpoint (job_id, scn) VALUES ($1, $2)
	ON CONFLICT (job_id) DO UPDATE SET scn = excluded.scn, updated_at = now()`

// pgName converts an Oracle identifier to the name used on the target. A
// name Oracle stores in upper case was written without quotes and is
// case-insensitive, which PostgreSQL expresses in lower case; a name with
// lower-case letters was quoted on purpose and is kept exactly.
func pgName(name string) string {
	if name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}
	return name
}

func qident(name string) string { return pgx.Identifier{name}.Sanitize() }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// clip shortens a name to PostgreSQL's 63-byte identifier limit.
func clip(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}
