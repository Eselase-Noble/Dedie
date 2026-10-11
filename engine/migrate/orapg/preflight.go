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
	if oracle.VersionNum(srcVer) < 120100 {
		r.Add("Source version", "fail", "Oracle 12c (12.1) or newer is required")
	}
	r.Add("Scope", "ok", fmt.Sprintf("%d table(s) from %s", len(plan.Tables), strings.Join(plan.Owners, ", ")))

	if len(plan.Unsupported) > 0 {
		r.Add("Column types", "fail", "these columns have a type that cannot be migrated yet: "+strings.Join(plan.Unsupported, ", ")+
			". Exclude their tables, or migrate them another way")
	}
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

	if _, err := currentSCN(ctx, src); err != nil {
		r.Add("Consistent read", "fail", err.Error())
	} else {
		// A flashback query on another user's table needs a privilege.
		var failed []string
		seen := map[string]bool{}
		for _, t := range plan.Tables {
			if seen[t.Owner] {
				continue
			}
			seen[t.Owner] = true
			scn, _ := currentSCN(ctx, src)
			var n int
			if err := src.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s AS OF SCN %d WHERE ROWNUM < 1", t.Source(), scn)).Scan(&n); err != nil && !undoGone(err) && !redefined(err) {
				failed = append(failed, t.Label()+": "+oracle.Friendly(err).Error())
			}
		}
		if len(failed) > 0 {
			r.Add("Consistent read", "fail", "the account cannot read these tables as of a past moment; grant it FLASHBACK on them or FLASHBACK ANY TABLE: "+strings.Join(failed, "; "))
		} else {
			var retention sql.NullString
			_ = src.QueryRowContext(ctx, `SELECT value FROM v$parameter WHERE name = 'undo_retention'`).Scan(&retention)
			detail := "all tables are read as of one moment with a flashback query; no lock is held on the source"
			if retention.Valid {
				detail += ". UNDO_RETENTION is " + retention.String + " seconds: if the copy of a table takes longer than the source keeps undo, it fails with \"snapshot too old\" and the setting has to be raised"
			}
			r.Add("Consistent read", "ok", detail)
		}
	}

	if cfg.Offline() {
		r.Add("Offline copy", "warn", "changes made to the source after the copy starts are not carried over; stop application writes first if the target must end up identical to the source")
	} else {
		checkMining(ctx, src, plan, cfg, r)
	}

	var encoding string
	_ = dst.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = current_database()`).Scan(&encoding)
	if encoding != "UTF8" {
		r.Add("Target encoding", "fail", "the target database uses "+encoding+"; create it with ENCODING 'UTF8' so it can hold any text from the source")
	}

	var existing []string
	for _, t := range plan.Tables {
		var found bool
		if err := dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2)`, t.Schema, t.Target).Scan(&found); err != nil {
			return nil, err
		}
		if found {
			existing = append(existing, t.Schema+"."+t.Target)
		}
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

// keyLogged returns the tables whose changes already carry their primary
// key in the redo stream, and whether the database logs keys for all.
func keyLogged(ctx context.Context, src oracle.Queryer, plan *Plan) (map[string]bool, bool) {
	var everywhere int
	_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$database WHERE supplemental_log_data_pk = 'YES' OR supplemental_log_data_all = 'YES'`).Scan(&everywhere)
	if everywhere == 0 {
		// A pluggable database can switch this on for itself.
		_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM dba_supplemental_logging WHERE primary_key = 'YES' OR all_column = 'YES'`).Scan(&everywhere)
	}
	logged := map[string]bool{}
	owners := make([]string, len(plan.Owners))
	for i, o := range plan.Owners {
		owners[i] = literal(o)
	}
	rows, err := query(ctx, src, `SELECT owner, table_name FROM all_log_groups
		WHERE owner IN (`+strings.Join(owners, ", ")+`) AND log_group_type IN ('PRIMARY KEY LOGGING', 'ALL COLUMN LOGGING')`)
	if err == nil {
		for _, r := range rows {
			logged[r[0]+"."+r[1]] = true
		}
	}
	return logged, everywhere > 0
}

// checkMining reports whether the source is set up for LogMiner and the
// account may use it.
func checkMining(ctx context.Context, src *sql.DB, plan *Plan, cfg migrate.Config, r *migrate.Report) {
	var minimal int
	_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$database WHERE supplemental_log_data_min IN ('YES', 'IMPLICIT')`).Scan(&minimal)
	if minimal == 0 {
		_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM dba_supplemental_logging WHERE minimal = 'YES'`).Scan(&minimal)
	}
	if minimal == 0 {
		r.Add("Supplemental logging", "fail", "following changes needs minimal supplemental logging on the source. A DBA switches it on, without a restart, with "+
			"ALTER DATABASE ADD SUPPLEMENTAL LOG DATA (in a pluggable database: ALTER PLUGGABLE DATABASE ADD SUPPLEMENTAL LOG DATA). "+
			"An offline migration needs none of this")
	} else {
		r.Add("Supplemental logging", "ok", "minimal supplemental logging is on")
	}

	var missing []string
	var logmining int
	_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_privs WHERE privilege = 'LOGMINING'`).Scan(&logmining)
	if logmining == 0 {
		missing = append(missing, "the LOGMINING privilege")
	}
	// Ending a session that was never started fails in a way that tells
	// "not allowed to call the package" from "nothing to end".
	if _, err := src.ExecContext(ctx, `BEGIN DBMS_LOGMNR.END_LOGMNR; END;`); err != nil && !strings.Contains(err.Error(), "ORA-01307") {
		missing = append(missing, "EXECUTE on DBMS_LOGMNR")
	}
	var n int
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$logmnr_contents WHERE 1 = 0`).Scan(&n); err != nil && strings.Contains(err.Error(), "ORA-00942") {
		missing = append(missing, "SELECT on V_$LOGMNR_CONTENTS")
	}
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$database`).Scan(&n); err != nil {
		missing = append(missing, "SELECT on V_$DATABASE")
	}
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$transaction`).Scan(&n); err != nil {
		missing = append(missing, "SELECT on V_$TRANSACTION")
	}
	if len(missing) == 0 {
		// Nothing shows that LogMiner works here like using it: read
		// the last few changes and stop.
		if err := trialMine(ctx, src); err != nil {
			r.Add("LogMiner access", "fail", err.Error())
			missing = nil
		}
	}
	if r.Checks[len(r.Checks)-1].Name == "LogMiner access" {
		// Reported above.
	} else if len(missing) > 0 {
		r.Add("LogMiner access", "fail", "to follow changes the source account needs "+strings.Join(missing, ", ")+
			". LOGMINING, SELECT ANY TRANSACTION, EXECUTE_CATALOG_ROLE and SELECT ANY DICTIONARY together cover it")
	} else {
		r.Add("LogMiner access", "ok", "the account can read the redo stream with LogMiner")
	}

	var mode sql.NullString
	_ = src.QueryRowContext(ctx, `SELECT log_mode FROM v$database`).Scan(&mode)
	if mode.String == "NOARCHIVELOG" {
		r.Add("Redo retention", "warn", "the source runs in NOARCHIVELOG mode, so only its online redo logs exist and they are reused once full. "+
			"The job keeps up while it runs, but if it is paused or falls behind for longer than the logs last it cannot continue and must start over. Run the source in ARCHIVELOG mode for a migration that matters")
	} else if mode.Valid {
		r.Add("Redo retention", "ok", "the source archives its redo logs; keep the archived logs until the migration has read them")
	}

	logged, everywhere := keyLogged(ctx, src, plan)
	var keyless, toLog []string
	for _, t := range plan.Tables {
		switch {
		case len(t.PK) == 0:
			keyless = append(keyless, t.Label())
		case !everywhere && !logged[t.Label()]:
			toLog = append(toLog, t.Label())
		}
	}
	if len(keyless) > 0 {
		r.Add("Tables without a primary key", "warn", "these tables are copied, but changes made to them after the copy cannot be followed: "+strings.Join(keyless, ", "))
	}
	if len(toLog) > 0 {
		after := "and switched off again when it finishes"
		if cfg.KeepSlot {
			after = "and left on afterwards"
		}
		r.Add("Primary key logging", "warn", fmt.Sprintf("when the job starts, primary key supplemental logging is switched on for %d table(s) (ALTER TABLE ... ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS), %s. The account must own the tables or hold ALTER on them", len(toLog), after))
	}
	if cfg.FenceSource {
		r.Add("Cutover", "warn", "making the source read-only at cutover is not available for Oracle; stop application writes yourself before requesting cutover")
	}
}

// trialMine opens and closes a LogMiner session over the most recent
// changes, to find out whether the account, the release and the place
// the connection leads to allow it.
func trialMine(ctx context.Context, src *sql.DB) error {
	conn, err := src.Conn(ctx)
	if err != nil {
		return oracle.Friendly(err)
	}
	defer conn.Close()
	scn, err := currentSCN(ctx, conn)
	if err != nil {
		return err
	}
	if err := startMiner(ctx, conn, max(scn-10, 1), scn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `BEGIN DBMS_LOGMNR.END_LOGMNR; END;`)
	return oracle.Friendly(err)
}
