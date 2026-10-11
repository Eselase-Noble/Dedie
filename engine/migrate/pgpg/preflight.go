package pgpg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg Config) (*Report, error) {
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close(ctx)
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close(ctx)
	plan, err := BuildPlan(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan)
}

// CheckSource records every finding about the source that does not depend on
// the kind of target: replication settings, privileges and the tables
// selected. It is shared by all pipelines that read from PostgreSQL.
func CheckSource(ctx context.Context, src *pgx.Conn, plan *Plan, r *Report) error {
	var walLevel string
	var canReplicate bool
	var slotsFree, sendersFree int
	if err := src.QueryRow(ctx, `SELECT current_setting('wal_level'),
			(SELECT rolreplication OR rolsuper FROM pg_roles WHERE rolname=current_user),
			current_setting('max_replication_slots')::int - (SELECT count(*) FROM pg_replication_slots),
			current_setting('max_wal_senders')::int - (SELECT count(*) FROM pg_stat_replication)`).
		Scan(&walLevel, &canReplicate, &slotsFree, &sendersFree); err != nil {
		return err
	}
	if walLevel == "logical" {
		r.Add("Source wal_level", "ok", "logical")
	} else {
		r.Add("Source wal_level", "fail", fmt.Sprintf("wal_level is %q; set wal_level = logical and restart the source server", walLevel))
	}
	if canReplicate {
		r.Add("Replication privilege", "ok", "source user may open replication connections")
	} else {
		r.Add("Replication privilege", "fail", "source user needs the REPLICATION attribute (ALTER ROLE ... REPLICATION)")
	}
	if slotsFree > 0 && sendersFree > 0 {
		r.Add("Replication capacity", "ok", fmt.Sprintf("%d free slot(s), %d free WAL sender(s)", slotsFree, sendersFree))
	} else {
		r.Add("Replication capacity", "fail", "no free replication slot or WAL sender; raise max_replication_slots / max_wal_senders")
	}

	if len(plan.Tables) == 0 {
		r.Add("Tables", "fail", "no table can be migrated with the current selection")
	} else {
		r.Add("Tables", "ok", fmt.Sprintf("%d table(s) will be migrated", len(plan.Tables)))
	}
	if len(plan.Missing) > 0 {
		r.Add("Selected tables", "fail", "these tables were selected but do not exist on the source: "+strings.Join(plan.Missing, ", "))
	}
	var blocking, skipped []string
	for _, s := range plan.Skipped {
		line := s.Schema + "." + s.Name + ": " + s.Reason
		if s.Blocking {
			blocking = append(blocking, line)
		} else {
			skipped = append(skipped, line)
		}
	}
	if len(blocking) > 0 {
		r.Add("Tables without a key", "fail", strings.Join(blocking, "\n"))
	}
	if len(skipped) > 0 {
		r.Add("Tables that will be skipped", "warn", strings.Join(skipped, "\n"))
	}
	return nil
}

func preflight(ctx context.Context, src, dst *pgx.Conn, plan *Plan) (*Report, error) {
	r := &Report{Tables: len(plan.Tables)}

	var srcVer, dstVer string
	var srcNum, dstNum int
	const verSQL = `SELECT current_setting('server_version'), current_setting('server_version_num')::int`
	if err := src.QueryRow(ctx, verSQL).Scan(&srcVer, &srcNum); err != nil {
		return nil, err
	}
	if err := dst.QueryRow(ctx, verSQL).Scan(&dstVer, &dstNum); err != nil {
		return nil, err
	}
	switch {
	case srcNum < 100000:
		r.Add("Source version", "fail", "PostgreSQL "+srcVer+" has no built-in logical replication; version 10 or newer is required")
	case dstNum < srcNum:
		r.Add("Versions", "fail", fmt.Sprintf("target (%s) is older than source (%s); downgrades are not supported", dstVer, srcVer))
	default:
		r.Add("Versions", "ok", fmt.Sprintf("%s → %s", srcVer, dstVer))
	}

	if err := CheckSource(ctx, src, plan, r); err != nil {
		return nil, err
	}

	// The target must not already contain the tables being migrated.
	names := make([]string, len(plan.Tables))
	for i, t := range plan.Tables {
		names[i] = t.Schema + "." + t.Name
	}
	var existing []string
	if err := collect(ctx, dst, &existing, `SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND n.nspname || '.' || c.relname = ANY($1) ORDER BY 1`, names); err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		r.Add("Target is empty", "fail", "these tables already exist on the target: "+strings.Join(existing, ", "))
	} else {
		r.Add("Target is empty", "ok", "no conflicting tables on the target")
	}

	var canCreate bool
	if err := dst.QueryRow(ctx, `SELECT has_database_privilege(current_database(), 'CREATE')`).Scan(&canCreate); err != nil {
		return nil, err
	}
	if canCreate {
		r.Add("Target privileges", "ok", "target user may create schemas")
	} else {
		r.Add("Target privileges", "fail", "target user needs CREATE on the target database")
	}

	var srcExt, missing []string
	if err := collect(ctx, src, &srcExt, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`); err != nil {
		return nil, err
	}
	if err := collect(ctx, dst, &missing, `SELECT e FROM unnest($1::text[]) e WHERE e NOT IN (SELECT name FROM pg_available_extensions)`, srcExt); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		r.Add("Extensions", "fail", "not installed on the target server: "+strings.Join(missing, ", "))
	} else if len(srcExt) > 0 {
		r.Add("Extensions", "ok", strings.Join(srcExt, ", "))
	}

	var srcEnc, dstEnc string
	const encSQL = `SELECT pg_encoding_to_char(encoding) || ' / ' || datcollate FROM pg_database WHERE datname=current_database()`
	_ = src.QueryRow(ctx, encSQL).Scan(&srcEnc)
	_ = dst.QueryRow(ctx, encSQL).Scan(&dstEnc)
	if srcEnc != dstEnc {
		r.Add("Encoding and collation", "warn", fmt.Sprintf("source is %s, target is %s; text ordering and index behaviour may differ", srcEnc, dstEnc))
	}

	// Objects the pipeline does not carry over. Reported, never silently dropped.
	var notMigrated []string
	count := func(label, sql string) {
		var n int
		if err := src.QueryRow(ctx, sql).Scan(&n); err == nil && n > 0 {
			notMigrated = append(notMigrated, fmt.Sprintf("%d %s", n, label))
		}
	}
	count("row-level security policies", `SELECT count(*) FROM pg_policies`)
	count("large objects", `SELECT count(*) FROM pg_largeobject_metadata`)
	count("event triggers", `SELECT count(*) FROM pg_event_trigger`)
	count("foreign tables", `SELECT count(*) FROM pg_foreign_table`)
	count("rewrite rules", `SELECT count(*) FROM pg_rules WHERE schemaname NOT IN ('pg_catalog','information_schema')`)
	if plan.VersionNum >= 110000 {
		count("user-defined aggregates", `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prokind='a' AND n.nspname NOT IN ('pg_catalog','information_schema')`)
	}
	notMigrated = append(notMigrated, "roles, ownership and GRANTs")
	r.Add("Not migrated", "warn", "recreate these on the target yourself: "+strings.Join(notMigrated, "; "))

	for _, side := range []struct {
		name string
		conn *pgx.Conn
	}{{"Source", src}, {"Target", dst}} {
		var ssl bool
		_ = side.conn.QueryRow(ctx, `SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()), false)`).Scan(&ssl)
		if !ssl {
			r.Add(side.name+" encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
		}
	}

	r.Finish()
	return r, nil
}
