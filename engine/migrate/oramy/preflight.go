package oramy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/oradef"
	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
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
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	version, err := targetVersion(ctx, dst)
	if err != nil {
		return nil, err
	}
	plan, err := BuildPlan(ctx, src, cfg, mysql.VersionNum(version))
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan, cfg)
}

// targetVersion reads the target's release, on which the translation of
// defaults, check constraints and the collation depend.
func targetVersion(ctx context.Context, dst *sql.DB) (string, error) {
	var version string
	if err := dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", fmt.Errorf("target: %s", message(err))
	}
	return version, nil
}

func preflight(ctx context.Context, src, dst *sql.DB, plan *Plan, cfg migrate.Config) (*migrate.Report, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	_, srcVer, err := oracle.Version(ctx, src)
	if err != nil {
		return nil, err
	}
	dstVer, err := targetVersion(ctx, dst)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(strings.ToLower(dstVer), "mariadb"):
		r.Add("Versions", "fail", "MariaDB is not supported as a target by this pipeline")
	case mysql.VersionNum(dstVer) < 80000:
		r.Add("Versions", "fail", "the target must be MySQL 8.0 or newer; the translated schema relies on its JSON and fractional-second types")
	default:
		r.Add("Versions", "ok", "Oracle "+srcVer+" → MySQL "+dstVer)
	}
	orasrc.CheckCatalog(plan.Catalog, srcVer, r)
	if long := plan.longNames(); len(long) > 0 {
		r.Add("Name length", "fail", "MySQL names are at most 64 characters; these are longer: "+strings.Join(long, ", "))
	}

	// The server may be set up to fold table names to lower case, and
	// then names that differ only in case are one name.
	var folds int
	_ = dst.QueryRowContext(ctx, "SELECT @@lower_case_table_names").Scan(&folds)
	collisions := plan.Collisions
	if folds != 0 {
		collisions = append(collisions[:len(collisions):len(collisions)], plan.CaseCollisions...)
	}
	if len(collisions) > 0 {
		r.Add("Table names", "fail", "MySQL has no schemas inside a database and does not tell the case of column names apart, so these would get the same name: "+
			strings.Join(collisions, "; ")+". Migrate the schemas separately, or exclude one of each pair")
	}
	if len(plan.LongKeys) > 0 {
		r.Add("Key length", "fail", fmt.Sprintf("MySQL cannot build the primary key of these tables: a key may be at most %d bytes long, counting four bytes for each character of text, "+
			"and cannot include TEXT or BLOB columns: %s. Exclude the tables, or migrate them another way", maxKeyBytes, strings.Join(plan.LongKeys, ", ")))
	}

	orasrc.CheckConsistentRead(ctx, src, plan.Catalog, r)
	orasrc.CheckChanges(ctx, src, plan.Catalog, cfg, r)

	var database, charset sql.NullString
	if err := dst.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return nil, fmt.Errorf("target: %s", message(err))
	}
	if !database.Valid {
		r.Add("Target database", "fail", "the target connection names no database; MySQL has no schemas inside one, so every table goes into the database of the connection")
	} else {
		_ = dst.QueryRowContext(ctx, `SELECT default_character_set_name FROM information_schema.schemata WHERE schema_name = DATABASE()`).Scan(&charset)
		if charset.String != "utf8mb4" {
			r.Add("Target encoding", "fail", "the target database's character set is "+charset.String+"; create it with CHARACTER SET utf8mb4 so it can hold any text from the source")
		}
		objects, err := existing(ctx, dst, plan)
		if err != nil {
			return nil, err
		}
		checkExisting(objects, database.String, cfg.Existing(), r)
	}

	if len(plan.Notes) > 0 {
		r.Add("Translated with changes", "warn", strings.Join(plan.Notes, "\n"))
	}
	var packet int64
	_ = dst.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&packet)
	r.Add("Behaviour that differs", "warn", strings.Join([]string{
		"MySQL has no schemas inside a database, so every table goes into the database of the target connection. Names Oracle stores in upper case become lower case, so EMPLOYEES.FIRST_NAME is employees.first_name.",
		"Tables are created with the " + plan.target.collation() + " collation, so text compares by exact characters, as it does in Oracle.",
		"An empty string is NULL in Oracle and a value of its own in MySQL. The data arrives as NULL; applications that write '' will now store it.",
		"DATE becomes DATETIME, because an Oracle DATE has a time of day. Timestamps with a time zone are stored in UTC. BOOLEAN becomes TINYINT(1) holding 0 or 1, and a CHAR value comes back without its trailing blanks.",
		"Values MySQL cannot store stop the migration rather than being altered: NaN and infinity in BINARY_FLOAT and BINARY_DOUBLE, and numbers too large for the column they go into. " +
			fmt.Sprintf("A row must also fit into one packet; this server's max_allowed_packet is %d MB.", packet>>20),
		"Identity columns become AUTO_INCREMENT; their counters are moved past the source's when the migration ends.",
		"TRUNCATE commits on its own in MySQL. A source transaction that truncates a table and changes others is applied in two steps, not atomically.",
		"Foreign keys are added at the very end, after the data has been validated. MySQL names foreign keys and check constraints per database, not per table.",
	}, "\n"))
	if len(plan.Views) > 0 {
		status, detail := viewsql.Describe(plan.Views)
		r.Add("Views", status, detail)
	}
	scope := ""
	if cfg.Scope() == "table" {
		scope = oradef.TableScopeNote
	}
	status, detail := storedcode.Describe(oracle.Kind, mysql.Kind, oradef.Convertible(plan.Code), cfg.CodeMode(), scope)
	r.Add("Stored code", status, detail)
	r.Add("Not migrated", "warn", sequencesLeftOut(plan.Catalog)+"materialized views, packages, synonyms, database links and partitioning are not translated; "+
		"a partitioned table arrives as one ordinary table. "+oradef.Describe+". Users, roles and grants are not copied")

	if !mysql.Encrypted(ctx, dst) {
		r.Add("Target encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	r.Finish()
	return r, nil
}

// sequencesLeftOut says which sequences stay behind. MySQL has none; its
// only counter is the AUTO_INCREMENT of a column.
func sequencesLeftOut(cat *orasrc.Catalog) string {
	if len(cat.Sequences) == 0 {
		return "MySQL has no sequences, so none are created; "
	}
	names := make([]string, 0, 10)
	for _, s := range cat.Sequences {
		if len(names) == cap(names) {
			names = append(names, "…")
			break
		}
		names = append(names, s.Owner+"."+s.Name)
	}
	return fmt.Sprintf("MySQL has no sequences, so the %d in scope are not created (%s), and a column default that takes its value from one is dropped; ", len(cat.Sequences), strings.Join(names, ", "))
}
