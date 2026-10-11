package orasrc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// The checks below are the source's share of a pipeline's preflight. Each
// adds its findings to the report and changes nothing on the source.

// CheckCatalog reports whether the source's release is one that can be
// migrated (version is what oracle.Version returns), what is in scope,
// and any column whose type cannot be carried over.
func CheckCatalog(cat *Catalog, version string, r *migrate.Report) {
	if oracle.VersionNum(version) < 120100 {
		r.Add("Source version", "fail", "Oracle 12c (12.1) or newer is required")
	}
	r.Add("Scope", "ok", fmt.Sprintf("%d table(s) from %s", len(cat.Tables), strings.Join(cat.Owners, ", ")))

	if len(cat.Unsupported) > 0 {
		r.Add("Column types", "fail", "these columns have a type that cannot be migrated yet: "+strings.Join(cat.Unsupported, ", ")+
			". Exclude their tables, or migrate them another way")
	}
}

// CheckConsistentRead reports whether the account can read the tables as
// of a past moment, which is how the copy sees all of them at one SCN.
func CheckConsistentRead(ctx context.Context, src *sql.DB, cat *Catalog, r *migrate.Report) {
	if _, err := CurrentSCN(ctx, src); err != nil {
		r.Add("Consistent read", "fail", err.Error())
		return
	}
	// A flashback query on another user's table needs a privilege.
	var failed []string
	seen := map[string]bool{}
	for _, t := range cat.Tables {
		if seen[t.Owner] {
			continue
		}
		seen[t.Owner] = true
		scn, _ := CurrentSCN(ctx, src)
		var n int
		if err := src.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s AS OF SCN %d WHERE ROWNUM < 1", t.Source(), scn)).Scan(&n); err != nil && !UndoGone(err) && !Redefined(err) {
			failed = append(failed, t.Label()+": "+oracle.Friendly(err).Error())
		}
	}
	if len(failed) > 0 {
		r.Add("Consistent read", "fail", "the account cannot read these tables as of a past moment; grant it FLASHBACK on them or FLASHBACK ANY TABLE: "+strings.Join(failed, "; "))
		return
	}
	var retention sql.NullString
	_ = src.QueryRowContext(ctx, `SELECT value FROM v$parameter WHERE name = 'undo_retention'`).Scan(&retention)
	detail := "all tables are read as of one moment with a flashback query; no lock is held on the source"
	if retention.Valid {
		detail += ". UNDO_RETENTION is " + retention.String + " seconds: if the copy of a table takes longer than the source keeps undo, it fails with \"snapshot too old\" and the setting has to be raised"
	}
	r.Add("Consistent read", "ok", detail)
}

// CheckChanges reports on what happens to changes made to the source
// after the copy starts: that an offline migration leaves them behind,
// or whether the source is ready to have them followed.
func CheckChanges(ctx context.Context, src *sql.DB, cat *Catalog, cfg migrate.Config, r *migrate.Report) {
	if cfg.Offline() {
		r.Add("Offline copy", "warn", "changes made to the source after the copy starts are not carried over; stop application writes first if the target must end up identical to the source")
		return
	}
	checkMining(ctx, src, cat, cfg, r)
}

// checkMining reports whether the source is set up for LogMiner and the
// account may use it.
func checkMining(ctx context.Context, src *sql.DB, cat *Catalog, cfg migrate.Config, r *migrate.Report) {
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

	logged, everywhere := KeyLogged(ctx, src, cat)
	var keyless, toLog []string
	for _, t := range cat.Tables {
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
	scn, err := CurrentSCN(ctx, conn)
	if err != nil {
		return err
	}
	if err := startMiner(ctx, conn, max(scn-10, 1), scn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `BEGIN DBMS_LOGMNR.END_LOGMNR; END;`)
	return oracle.Friendly(err)
}
