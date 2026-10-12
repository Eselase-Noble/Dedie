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
	"dedie/engine/migrate/mymy"
	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/viewsql"
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
	return preflight(ctx, src, dst, plan, cfg)
}

func preflight(ctx context.Context, src *pgx.Conn, dst *sql.DB, plan *Plan, cfg migrate.Config) (*migrate.Report, error) {
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
	case srcNum < 100000 && !cfg.Offline():
		r.Add("Versions", "fail", "PostgreSQL "+srcVer+" has no built-in logical replication; version 10 or newer is required")
	case strings.Contains(strings.ToLower(dstVer), "mariadb"):
		r.Add("Versions", "fail", "MariaDB is not supported as a target by this pipeline")
	case mysql.VersionNum(dstVer) < 80000:
		r.Add("Versions", "fail", "the target must be MySQL 8.0 or newer; the translated schema relies on its JSON and fractional-second types")
	default:
		r.Add("Versions", "ok", "PostgreSQL "+srcVer+" → MySQL "+dstVer)
	}
	if err := pgpg.CheckSource(ctx, src, plan.Plan, cfg, r); err != nil {
		return nil, err
	}

	if len(plan.Collisions) > 0 {
		r.Add("Table names", "fail", "MySQL has no schemas inside a database, and these tables would get the same name: "+
			strings.Join(plan.Collisions, "; ")+". Migrate the schemas separately, or exclude one of each pair")
	}
	// The target database must not already hold the tables being migrated,
	// nor anything else under a name the plan is going to take.
	found, err := mymy.FindExisting(ctx, dst, plan.objects())
	if err != nil {
		return nil, err
	}
	mymy.ReportExisting(r, found, cfg.Existing())

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
	if len(plan.Views) > 0 {
		status, detail := viewsql.Describe(plan.Views)
		r.Add("Views", status, detail)
	}
	r.Add("Not migrated", "warn", "functions, triggers, check constraints, materialized views, sequences that are not a column default, and types are specific to PostgreSQL and are not translated. "+
		"The definitions of the functions, triggers, constraints and materialized views, and of the views that are left out, can be had as one script to convert by hand. Roles and privileges are not copied")

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
