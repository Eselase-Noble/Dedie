// Package mymy migrates one MySQL database into another, typically on a
// newer server version, while the source stays online.
//
// It follows the same five phases as the other pipelines. The initial copy
// reads every table inside transactions that were opened at one binary log
// position, and the stream then replays row events from exactly that
// position, so no change is lost or applied twice. While streaming, each
// source transaction is applied as one target transaction that also records
// its binary log position in a checkpoint table, which makes a restart after
// a crash exactly-once.
//
// Requirements on the source: binary logging in ROW format with full row
// images, and an account with SELECT, RELOAD, REPLICATION SLAVE and
// REPLICATION CLIENT. An offline job copies from the snapshot and finishes
// without following the binary log: it needs neither binary logging nor the
// replication privileges, and RELOAD only to copy with more than one worker.
package mymy

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

func init() { migrate.Register(mysql.Kind, mysql.Kind, pipeline{}) }

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

// Position is a point in the source's binary log.
type Position struct {
	File string `json:"file"`
	Pos  uint32 `json:"pos"`
}

func (p Position) String() string {
	if p.File == "" {
		return ""
	}
	return p.File + ":" + itoa(uint64(p.Pos))
}

// After reports whether p is at or beyond q. Binary log file names share a
// base name and a zero-padded sequence number, so they order as strings.
func (p Position) reached(q Position) bool {
	if p.File != q.File {
		return len(p.File) > len(q.File) || (len(p.File) == len(q.File) && p.File > q.File)
	}
	return p.Pos >= q.Pos
}

// State is the durable progress of a job. The JSON names match the other
// pipelines so the interface can show any job the same way.
type State struct {
	SchemaDone     bool      `json:"schema_done"`
	SnapshotDone   bool      `json:"snapshot_done"`
	IndexingDone   bool      `json:"indexing_done"`
	Fenced         bool      `json:"fenced"`
	Start          Position  `json:"start"`
	Applied        Position  `json:"applied"`
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

	// What the job does about objects that were on the target before it
	// started. It is decided once, in the schema phase, from the policy
	// chosen for the job: later the job's own objects exist as well, and
	// nothing on the target tells the two apart.
	//
	// SkippedTables are the tables left out of the migration because the
	// target's are kept, named as the job's table selection names them.
	SkippedTables []string `json:"skipped_tables,omitempty"`
	// KeptObjects are the other objects that are kept: the plan does not
	// create them.
	KeptObjects []string `json:"kept_objects,omitempty"`
	// DropFirst are the statements that remove what the job replaces.
	DropFirst []string `json:"drop_first,omitempty"`
}

// checkpointTable lives in the target database while a job runs.
const checkpointTable = "dedie_checkpoint"

const checkpointDDL = "CREATE TABLE IF NOT EXISTS `" + checkpointTable + "` (" +
	"job_id VARCHAR(64) NOT NULL PRIMARY KEY, binlog_file VARCHAR(255) NOT NULL, binlog_pos BIGINT UNSIGNED NOT NULL, " +
	"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB"

const checkpointUpsert = "INSERT INTO `" + checkpointTable + "` (job_id, binlog_file, binlog_pos) VALUES (?, ?, ?) " +
	"ON DUPLICATE KEY UPDATE binlog_file = VALUES(binlog_file), binlog_pos = VALUES(binlog_pos)"

// targetParams are the session settings for connections that write to the
// target. Zero values in AUTO_INCREMENT columns and zero dates are data to be
// copied faithfully, not input to be reinterpreted or rejected.
func targetParams(extra map[string]string) map[string]string {
	p := map[string]string{"sql_mode": "'NO_AUTO_VALUE_ON_ZERO'"}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// QueryStrings scans every column of a query's rows as text. It suits the SHOW
// statements, whose column sets differ between server versions.
func QueryStrings(ctx context.Context, q Querier, query string, args ...any) ([][]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make([]string, len(raw))
		for i, v := range raw {
			row[i] = string(v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// BinlogStatus returns the source's current binary log position.
func BinlogStatus(ctx context.Context, q Querier) (Position, error) {
	// SHOW MASTER STATUS was renamed in MySQL 8.2 and removed in 8.4.
	rows, err := QueryStrings(ctx, q, "SHOW BINARY LOG STATUS")
	if err != nil {
		rows, err = QueryStrings(ctx, q, "SHOW MASTER STATUS")
	}
	if err != nil {
		return Position{}, err
	}
	if len(rows) == 0 || len(rows[0]) < 2 {
		return Position{}, errNoBinlog
	}
	var pos uint64
	for _, c := range rows[0][1] {
		pos = pos*10 + uint64(c-'0')
	}
	return Position{File: rows[0][0], Pos: uint32(pos)}, nil
}
