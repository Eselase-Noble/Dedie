// Package oramy migrates an Oracle database into MySQL, either as a
// one-time copy or while the source stays in use.
//
// The schema is translated rather than copied: every Oracle type maps to
// a MySQL type that can hold all of its values, and whatever cannot be
// carried over (sequences, PL/SQL, expression defaults MySQL does not
// know) is reported instead of guessed. Views are translated where every
// construct they use has an equivalent that means the same, created once
// the data is in, and kept only if they then return what the source's
// views return; see package viewsql. MySQL has no schemas inside a
// database, so every table goes into the database the target connection
// names, and names that Oracle stores in upper case are written in lower
// case, which is how they are usually written in MySQL.
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
// other pipelines out of Oracle share. What is here is the MySQL side.
package oramy

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

func init() { migrate.Register(oracle.Kind, mysql.Kind, pipeline{}) }

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
	// ExistingDecided says the job has settled what happens to the
	// objects that were on the target before it created anything. The
	// decision is taken once and kept, because afterwards the job's own
	// tables exist there too.
	ExistingDecided bool `json:"existing_decided,omitempty"`
	// SkippedTables lists the source tables, as OWNER.NAME, left out of
	// the migration because the target already had a table of their name
	// and existing objects are skipped.
	SkippedTables []string `json:"skipped_tables,omitempty"`
	// SkippedObjects lists the foreign keys and check constraints not
	// created for the same reason, each as its kind and name.
	SkippedObjects []string `json:"skipped_objects,omitempty"`

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

// checkpointTable lives in the target database while a job runs. Jobs
// that share a target database share the table, one row each.
const checkpointTable = "dedie_oracle_checkpoint"

const checkpointDDL = "CREATE TABLE IF NOT EXISTS `" + checkpointTable + "` (" +
	"job_id VARCHAR(64) NOT NULL PRIMARY KEY, scn BIGINT NOT NULL, " +
	"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB"

const checkpointUpsert = "INSERT INTO `" + checkpointTable + "` (job_id, scn) VALUES (?, ?) " +
	"ON DUPLICATE KEY UPDATE scn = VALUES(scn)"

// targetParams are the session settings of every connection that writes
// to the target. Strict mode makes MySQL reject a value that does not fit
// its column instead of silently altering it, and NO_AUTO_VALUE_ON_ZERO
// lets an identity value of 0 arrive as 0. Naming the whole mode also
// switches off whatever the server's default would add, so a value is
// read and compared the same way on every server.
//
// The session time zone needs no entry here: mysql.Open sets it to UTC on
// every connection. Dates and timestamps go into DATETIME columns, which
// MySQL never shifts, and are written as text that is already in UTC
// where the source value had a zone.
var targetParams = map[string]string{
	"sql_mode": "'STRICT_ALL_TABLES,NO_AUTO_VALUE_ON_ZERO'",
}

// applierParams are the settings of the one connection that applies
// followed changes: those of any writer, with foreign key checks off.
//
// Foreign keys are only added when a migration ends, so normally there
// are none to check. They can exist while changes are applied in one
// case: a job that was interrupted while adding them goes back to
// following when it is started again. Rows are stored table by table and
// whole, by deleting and inserting them, so with the checks on a parent
// could not be replaced while its children exist, and replacing it would
// set off ON DELETE CASCADE for rows the source never deleted.
var applierParams = map[string]string{
	"sql_mode":           targetParams["sql_mode"],
	"foreign_key_checks": "0",
}

// features says what the target's release can do. The version is in the
// form mysql.VersionNum returns: 80016 for 8.0.16.
type features struct{ version int }

// exprDefaults reports whether a column default may be an expression.
func (f features) exprDefaults() bool { return f.version >= 80013 }

// checks reports whether check constraints are enforced. Older releases
// parse them and throw them away.
func (f features) checks() bool { return f.version >= 80016 }

// collation is the one every table is created with. Oracle compares text
// byte by byte and counts trailing blanks, so that 'a' and 'a ' are two
// values to a unique index. utf8mb4_0900_bin does the same; the older
// utf8mb4_bin compares by code point too but ignores trailing blanks.
func (f features) collation() string {
	if f.version >= 80017 {
		return "utf8mb4_0900_bin"
	}
	return "utf8mb4_bin"
}

// myName converts an Oracle identifier to the name used on the target. A
// name Oracle stores in upper case was written without quotes and is
// case-insensitive, which is written in lower case on the target, as the
// other pipelines out of Oracle do; a name with lower-case letters was
// quoted on purpose and is kept exactly.
func myName(name string) string {
	if name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}
	return name
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// maxNameLength is how many characters a MySQL identifier may have.
const maxNameLength = 64

// clip shortens a name to MySQL's identifier limit.
func clip(name string) string {
	if r := []rune(name); len(r) > maxNameLength {
		return string(r[:maxNameLength])
	}
	return name
}
