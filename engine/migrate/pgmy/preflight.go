package pgmy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration, including every place
// where the translated schema differs in meaning from the original.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close(ctx)
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	plan, err := BuildPlan(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan)
}

func preflight(ctx context.Context, src *pgx.Conn, dst *sql.DB, plan *Plan) (*migrate.Report, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	var srcVer, dstVer string
	var srcNum int
	if err := src.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int`).Scan(&srcVer, &srcNum); err != nil {
		return nil, err
	}
	if err := dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&dstVer); err != nil {
		return nil, err
	}
	switch {
	case srcNum < 100000:
		r.Add("Versions", "fail", "PostgreSQL "+srcVer+" has no built-in logical replication; version 10 or newer is required")
	case strings.Contains(strings.ToLower(dstVer), "mariadb"):
		r.Add("Versions", "fail", "MariaDB is not supported as a target by this pipeline")
	case mysql.VersionNum(dstVer) < 80000:
		r.Add("Versions", "fail", "the target must be MySQL 8.0 or newer; the translated schema relies on its JSON and fractional-second types")
	default:
		r.Add("Versions", "ok", "PostgreSQL "+srcVer+" → MySQL "+dstVer)
	}
	if err := pgpg.CheckSource(ctx, src, plan.Plan, r); err != nil {
		return nil, err
	}

	if len(plan.Collisions) > 0 {
		r.Add("Table names", "fail", "MySQL has no schemas inside a database, and these tables would get the same name: "+
			strings.Join(plan.Collisions, "; ")+". Migrate the schemas separately, or exclude one of each pair")
	}
	var dstDB sql.NullString
	if err := dst.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dstDB); err != nil {
		return nil, err
	}
	args := []any{dstDB.String}
	var marks []string
	for _, t := range plan.Tables {
		args = append(args, t.Target)
		marks = append(marks, "?")
	}
	if len(marks) > 0 {
		rows, err := dst.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
			WHERE table_schema = ? AND table_name IN (`+strings.Join(marks, ",")+`) ORDER BY 1`, args...)
		if err != nil {
			return nil, err
		}
		var existing []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			existing = append(existing, name)
		}
		rows.Close()
		if len(existing) > 0 {
			r.Add("Target is empty", "fail", "these tables already exist in the target database: "+strings.Join(existing, ", "))
		} else {
			r.Add("Target is empty", "ok", "no conflicting tables in target database "+dstDB.String)
		}
	}

	if len(plan.Notes) > 0 {
		r.Add("Translated with changes", "warn", strings.Join(plan.Notes, "\n"))
	}
	r.Add("Behaviour that differs", "warn", strings.Join([]string{
		"Tables are created with the utf8mb4_bin collation, so text compares by exact bytes as it does in PostgreSQL.",
		"Values MySQL cannot store stop the migration rather than being altered: infinite dates and timestamps, NaN, dates before year 1000, and numbers beyond DECIMAL(65,30).",
		"Boolean columns become TINYINT(1) holding 0 or 1.",
		"Schemas are flattened: with more than one schema, tables outside public are named schema_table.",
		"TRUNCATE commits on its own in MySQL. A source transaction that truncates a table and changes others is applied in two steps, not atomically.",
	}, "\n"))
	r.Add("Not migrated", "warn", "views, functions, triggers, check constraints, sequences that are not a column default, and types are specific to PostgreSQL and are not translated; roles and privileges are not copied")

	var ssl bool
	_ = src.QueryRow(ctx, `SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&ssl)
	if !ssl {
		r.Add("Source encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	if !mysql.Encrypted(ctx, dst) {
		r.Add("Target encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	r.Finish()
	return r, nil
}
