// Package mypg migrates a MySQL database into PostgreSQL while the source
// stays online.
//
// It reads the source exactly as the MySQL to MySQL pipeline does: a
// consistent snapshot taken at a known binary log position, then the row
// events from that position onwards. What differs is the target. The schema
// is translated rather than copied, values are converted between the two
// type systems, and changes are applied to PostgreSQL together with a
// checkpoint of the binary log position. An offline job ends after the
// snapshot and never reads the binary log.
//
// Translation is deliberately conservative: every type maps to one that can
// hold all of its values, and anything that cannot be carried over (views,
// routines, triggers, expression defaults) is reported instead of guessed.
package mypg

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

func init() { migrate.Register(mysql.Kind, postgres.Kind, pipeline{}) }

// pipeline adapts this package to the engine's pipeline registry.
type pipeline struct{}

func (pipeline) Preflight(ctx context.Context, src, dst connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	return Preflight(ctx, src, dst, cfg)
}

func (pipeline) Run(ctx context.Context, jobID string, cfg migrate.Config, state json.RawMessage, src, dst connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	var st mymy.State
	if len(state) > 0 {
		_ = json.Unmarshal(state, &st)
	}
	return Run(ctx, jobID, cfg, st, src, dst, rep)
}

func (pipeline) Preview(ctx context.Context, src, dst connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	return Preview(ctx, src, dst, cfg)
}

// targetSchema is where the migrated tables are created.
const targetSchema = "public"

// metaSchema holds the checkpoint table on the target while a job runs.
const metaSchema = "dedie"

const checkpointDDL = `CREATE SCHEMA IF NOT EXISTS ` + metaSchema + `;
CREATE TABLE IF NOT EXISTS ` + metaSchema + `.mysql_checkpoint (
	job_id text PRIMARY KEY, binlog_file text NOT NULL, binlog_pos bigint NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

const checkpointUpsert = `INSERT INTO ` + metaSchema + `.mysql_checkpoint (job_id, binlog_file, binlog_pos) VALUES ($1, $2, $3)
	ON CONFLICT (job_id) DO UPDATE SET binlog_file = excluded.binlog_file, binlog_pos = excluded.binlog_pos, updated_at = now()`

func qname(table string) string { return pgx.Identifier{targetSchema, table}.Sanitize() }

func qident(name string) string { return pgx.Identifier{name}.Sanitize() }
