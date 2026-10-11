package orapg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/pgpg"
)

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration, including every
// place where the translated schema differs in meaning from the original.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(ctx)
	plan, err := BuildPlan(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan, cfg)
}

func preflight(ctx context.Context, src *sql.DB, dst *pgx.Conn, plan *Plan, cfg migrate.Config) (*migrate.Report, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	_, srcVer, err := oracle.Version(ctx, src)
	if err != nil {
		return nil, err
	}
	var dstVer string
	if err := dst.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&dstVer); err != nil {
		return nil, err
	}
	r.Add("Versions", "ok", "Oracle "+srcVer+" → PostgreSQL "+dstVer)
	orasrc.CheckCatalog(plan.Catalog, srcVer, r)
	var long []string
	for _, t := range plan.Tables {
		if len(t.Target) > 63 {
			long = append(long, t.Label())
		}
		for _, c := range t.Cols {
			if len(c.Target) > 63 {
				long = append(long, t.Label()+"."+c.Name)
			}
		}
	}
	if len(long) > 0 {
		r.Add("Name length", "fail", "PostgreSQL names are at most 63 bytes; these are longer: "+strings.Join(long, ", "))
	}

	orasrc.CheckConsistentRead(ctx, src, plan.Catalog, r)
	orasrc.CheckChanges(ctx, src, plan.Catalog, cfg, r)

	var encoding string
	_ = dst.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = current_database()`).Scan(&encoding)
	if encoding != "UTF8" {
		r.Add("Target encoding", "fail", "the target database uses "+encoding+"; create it with ENCODING 'UTF8' so it can hold any text from the source")
	}

	// The target must not already hold the tables being migrated, nor
	// anything else under a name the plan is going to take.
	found, err := pgpg.FindExisting(ctx, dst, plan.claims())
	if err != nil {
		return nil, err
	}
	pgpg.ReportExisting(r, found, cfg.Existing(), "on the target")
	var canCreate bool
	if err := dst.QueryRow(ctx, `SELECT has_database_privilege(current_database(), 'CREATE')`).Scan(&canCreate); err != nil {
		return nil, err
	}
	if !canCreate {
		r.Add("Target privileges", "fail", "the target user needs CREATE on the database, to create a schema for each Oracle schema")
	}

	if len(plan.Notes) > 0 {
		r.Add("Translated with changes", "warn", strings.Join(plan.Notes, "\n"))
	}
	r.Add("Behaviour that differs", "warn", strings.Join([]string{
		"Names Oracle stores in upper case become lower case, so EMPLOYEES.FIRST_NAME is employees.first_name; each Oracle schema becomes a PostgreSQL schema.",
		"An empty string is NULL in Oracle and a value of its own in PostgreSQL. The data arrives as NULL; applications that write '' will now store it.",
		"DATE becomes timestamp(0), because an Oracle DATE has a time of day. NUMBER without a precision becomes numeric.",
		"Foreign keys are added at the very end, after the data has been validated.",
	}, "\n"))
	r.Add("Not migrated", "warn", "views, materialized views, PL/SQL (procedures, functions, packages, triggers), synonyms, database links and partitioning are not translated; a partitioned table arrives as one ordinary table. Users, roles and grants are not copied")

	var ssl bool
	_ = dst.QueryRow(ctx, `SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&ssl)
	if !ssl {
		r.Add("Target encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	r.Finish()
	return r, nil
}
