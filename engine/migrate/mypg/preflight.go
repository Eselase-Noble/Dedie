package mypg

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
)

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration, including every place
// where the translated schema differs in meaning from the original.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(ctx)
	plan, err := BuildPlan(ctx, src, srcCfg.Database, cfg)
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan, cfg)
}

func preflight(ctx context.Context, src *sql.DB, dst *pgx.Conn, plan *Plan, cfg migrate.Config) (*migrate.Report, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	srcVer, err := mymy.CheckSource(ctx, src, plan.Plan, cfg, r)
	if err != nil {
		return nil, err
	}
	var dstVer string
	if err := dst.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&dstVer); err != nil {
		return nil, err
	}
	r.Add("Versions", "ok", "MySQL "+srcVer+" → PostgreSQL "+dstVer)

	var encoding string
	_ = dst.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = current_database()`).Scan(&encoding)
	if encoding != "UTF8" {
		r.Add("Target encoding", "fail", "the target database uses "+encoding+"; create it with ENCODING 'UTF8' so it can hold any text from the source")
	}

	var unsupported []string
	for _, t := range plan.Tables {
		for _, c := range t.Cols {
			if !supportedCharsets[c.Charset] {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s (%s)", t.Name, c.Name, c.Charset))
			}
		}
	}
	if len(unsupported) > 0 {
		r.Add("Character sets", "fail", "these columns use a character set that cannot be converted yet: "+strings.Join(unsupported, ", ")+
			". Supported: utf8mb4, utf8, latin1, ascii")
	}

	// The target must not already hold the tables being migrated, nor
	// anything else under a name the plan is going to take.
	found, err := pgpg.FindExisting(ctx, dst, plan.claims())
	if err != nil {
		return nil, err
	}
	pgpg.ReportExisting(r, found, cfg.Existing(), "in schema "+targetSchema+" on the target")

	var canCreate bool
	if err := dst.QueryRow(ctx, `SELECT has_database_privilege(current_database(), 'CREATE') AND has_schema_privilege($1, 'CREATE')`, targetSchema).Scan(&canCreate); err != nil {
		return nil, err
	}
	if !canCreate {
		r.Add("Target privileges", "fail", "the target user needs CREATE on the database and on schema "+targetSchema)
	}

	if len(plan.Notes) > 0 {
		r.Add("Translated with changes", "warn", strings.Join(plan.Notes, "\n"))
	}
	r.Add("Behaviour that differs", "warn", strings.Join([]string{
		"Text comparison is case-sensitive in PostgreSQL; most MySQL collations are not. Unique keys and lookups that relied on case-insensitive matching behave differently.",
		"Zero dates (0000-00-00) do not exist in PostgreSQL and become NULL; a NOT NULL column holding one will stop the migration.",
		"TINYINT(1) columns become smallint, not boolean, so no value is lost.",
		"ENUM and SET columns become text; ENUM keeps its list of allowed values as a CHECK constraint.",
	}, "\n"))
	r.Add("Not migrated", "warn", "views, stored routines, triggers and scheduled events are written in MySQL's dialect and are not translated; user accounts and privileges are not copied")

	var ssl bool
	_ = dst.QueryRow(ctx, `SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&ssl)
	if !ssl {
		r.Add("Target encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	r.Finish()
	return r, nil
}
