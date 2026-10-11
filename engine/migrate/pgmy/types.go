// Package pgmy migrates a PostgreSQL database into MySQL while the source
// stays online.
//
// It reads the source exactly as the PostgreSQL to PostgreSQL pipeline does:
// a replication slot created together with an exported snapshot, then the
// logical replication stream from that point. What differs is the target.
// The schema is translated rather than copied, values are converted between
// the two type systems, and changes are applied to MySQL together with a
// checkpoint of the replication position.
//
// MySQL has no schemas inside a database, so tables from several PostgreSQL
// schemas share one namespace on the target; see tableName.
package pgmy

import (
	"context"
	"encoding/json"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

func init() { migrate.Register(postgres.Kind, mysql.Kind, pipeline{}) }

// pipeline adapts this package to the engine's pipeline registry.
type pipeline struct{}

func (pipeline) Preflight(ctx context.Context, src, dst connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	return Preflight(ctx, src, dst, cfg)
}

func (pipeline) Run(ctx context.Context, jobID string, cfg migrate.Config, state json.RawMessage, src, dst connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	var st pgpg.State
	if len(state) > 0 {
		_ = json.Unmarshal(state, &st)
	}
	return Run(ctx, jobID, cfg, st, src, dst, rep)
}

// checkpointTable lives in the target database while a job runs.
const checkpointTable = "dedie_pg_checkpoint"

const checkpointDDL = "CREATE TABLE IF NOT EXISTS `" + checkpointTable + "` (" +
	"job_id VARCHAR(64) NOT NULL PRIMARY KEY, commit_lsn VARCHAR(32) NOT NULL, end_lsn VARCHAR(32) NOT NULL, " +
	"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB"

const checkpointUpsert = "INSERT INTO `" + checkpointTable + "` (job_id, commit_lsn, end_lsn) VALUES (?, ?, ?) " +
	"ON DUPLICATE KEY UPDATE commit_lsn = VALUES(commit_lsn), end_lsn = VALUES(end_lsn)"

// targetParams are the session settings for connections that write to the
// target. Strict mode makes MySQL reject a value that does not fit its
// column instead of silently shortening it. Foreign-key checks are off
// because PostgreSQL replicates the rows changed by a cascade itself; a
// second cascade on the target would collide with them.
var targetParams = map[string]string{
	"sql_mode":           "'STRICT_TRANS_TABLES,NO_AUTO_VALUE_ON_ZERO'",
	"foreign_key_checks": "0",
}
