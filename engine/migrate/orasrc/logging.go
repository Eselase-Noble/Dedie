package orasrc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// KeyLogged returns the tables whose changes already carry their primary
// key in the redo stream, by label, and whether the database logs keys
// for all.
func KeyLogged(ctx context.Context, src oracle.Queryer, cat *Catalog) (map[string]bool, bool) {
	var everywhere int
	_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$database WHERE supplemental_log_data_pk = 'YES' OR supplemental_log_data_all = 'YES'`).Scan(&everywhere)
	if everywhere == 0 {
		// A pluggable database can switch this on for itself.
		_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM dba_supplemental_logging WHERE primary_key = 'YES' OR all_column = 'YES'`).Scan(&everywhere)
	}
	logged := map[string]bool{}
	rows, err := query(ctx, src, `SELECT owner, table_name FROM all_log_groups
		WHERE owner IN (`+cat.ownerList()+`) AND log_group_type IN ('PRIMARY KEY LOGGING', 'ALL COLUMN LOGGING')`)
	if err == nil {
		for _, r := range rows {
			logged[r[0]+"."+r[1]] = true
		}
	}
	return logged, everywhere > 0
}

// LogKeys makes the redo stream carry the primary key of every changed
// row, for the tables where it does not already. Without the key a
// change cannot be matched to its row on the target. It must happen
// before the copy is taken, so that every change after it is covered.
//
// switched is called with the label of each table as soon as its logging
// is on, so that the job can record it durably before the next table is
// touched and switch off again exactly what it switched on, even if it
// is interrupted halfway. Those labels are what UnlogKeys takes.
func LogKeys(ctx context.Context, src *sql.DB, cat *Catalog, switched func(table string)) error {
	logged, everywhere := KeyLogged(ctx, src, cat)
	if everywhere || MiningBlocked(ctx, src) != "" {
		// Where LogMiner cannot be used, changes are followed through
		// row versions, which need no logging.
		return nil
	}
	conn, err := src.Conn(ctx)
	if err != nil {
		return oracle.Friendly(err)
	}
	defer conn.Close()
	// Wait briefly for transactions in flight instead of failing at once.
	conn.ExecContext(ctx, `ALTER SESSION SET DDL_LOCK_TIMEOUT = 30`)
	for _, t := range cat.Tables {
		if len(t.PK) == 0 || logged[t.Label()] || unmined(t) {
			continue
		}
		if _, err := conn.ExecContext(ctx, "ALTER TABLE "+t.Source()+" ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS"); err != nil {
			if HasCode(err, "ORA-01031") {
				// Not allowed to: the job follows row versions instead.
				return nil
			}
			if strings.Contains(err.Error(), "ORA-00054") {
				return fmt.Errorf("switch on primary key logging for %s: a transaction has held the table for more than 30 seconds, and the change needs it for an instant. "+
					"Start the job again when that transaction has ended, or switch the logging on yourself beforehand with ALTER TABLE ... ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS", t.Label())
			}
			return fmt.Errorf("switch on primary key logging for %s: %w", t.Label(), oracle.Friendly(err))
		}
		switched(t.Label())
	}
	return nil
}

// UnlogKeys switches off the logging LogKeys switched on for the given
// tables, and returns those it is still on for. The error is set only
// when the source could not be reached at all.
func UnlogKeys(ctx context.Context, src *sql.DB, tables []string) (left []string, err error) {
	conn, err := src.Conn(ctx)
	if err != nil {
		return tables, oracle.Friendly(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, `ALTER SESSION SET DDL_LOCK_TIMEOUT = 30`)
	for _, label := range tables {
		owner, name, _ := strings.Cut(label, ".")
		_, err := conn.ExecContext(ctx, "ALTER TABLE "+oracle.QName(owner, name)+" DROP SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS")
		// ORA-32587: the log group is already gone; ORA-00942: so is the table.
		if err != nil && !strings.Contains(err.Error(), "ORA-32587") && !strings.Contains(err.Error(), "ORA-00942") {
			left = append(left, label)
		}
	}
	return left, nil
}
