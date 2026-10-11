package orapg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedie/engine/connector/oracle"
)

// sessionFormats fix how the session prints and reads dates and numbers.
// LogMiner writes values with them, and keys read from the redo stream
// are handed back to Oracle as text, so both directions must agree.
var sessionFormats = []string{
	`ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS'`,
	`ALTER SESSION SET NLS_TIMESTAMP_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF9'`,
	`ALTER SESSION SET NLS_TIMESTAMP_TZ_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM'`,
	`ALTER SESSION SET NLS_NUMERIC_CHARACTERS = '.,'`,
}

// pinned takes one connection out of the pool and sets its formats.
func pinned(ctx context.Context, src *sql.DB) (*sql.Conn, error) {
	conn, err := src.Conn(ctx)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	for _, stmt := range sessionFormats {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			conn.Close()
			return nil, oracle.Friendly(err)
		}
	}
	return conn, nil
}

// currentSCN reads the database's system change number. V$DATABASE is
// the usual place; an account without access to it may still be allowed
// to ask DBMS_FLASHBACK.
func currentSCN(ctx context.Context, src oracle.Queryer) (int64, error) {
	var scn int64
	err := src.QueryRowContext(ctx, `SELECT current_scn FROM v$database`).Scan(&scn)
	if err == nil {
		return scn, nil
	}
	if err2 := src.QueryRowContext(ctx, `SELECT DBMS_FLASHBACK.GET_SYSTEM_CHANGE_NUMBER FROM dual`).Scan(&scn); err2 == nil {
		return scn, nil
	}
	return 0, fmt.Errorf("cannot read the current SCN (%s); grant the account SELECT on V_$DATABASE or EXECUTE on DBMS_FLASHBACK", oracle.Friendly(err))
}

// oldestOpen returns the SCN at which the oldest transaction still open
// began, or 0 when there is none or the account cannot see them.
func oldestOpen(ctx context.Context, src oracle.Queryer) int64 {
	var scn sql.NullInt64
	_ = src.QueryRowContext(ctx, `SELECT MIN(start_scn) FROM v$transaction WHERE start_scn > 0`).Scan(&scn)
	return scn.Int64
}

// keyWhere is the condition that selects one row by primary key, with
// the key bound as text. Fixed-width and binary keys need a conversion
// to compare equal.
func keyWhere(t *Table) string {
	conds := make([]string, len(t.PK))
	for i, col := range t.PK {
		c := t.Cols[col]
		bind := fmt.Sprintf(":%d", i+1)
		switch {
		case c.Kind == kBinary:
			bind = "HEXTORAW(" + bind + ")"
		case c.DataType == "CHAR" || c.DataType == "NCHAR":
			bind = fmt.Sprintf("CAST(%s AS %s(%d))", bind, c.DataType, max(c.CharLength, 1))
		}
		conds[i] = oracle.Quote(c.Name) + " = " + bind
	}
	return strings.Join(conds, " AND ")
}

// errUndoGone reports that Oracle no longer keeps the undo needed to
// read the source as of a past moment.
var errUndoGone = errors.New("the source no longer holds the undo data for that moment")

// errRedefined reports that a table cannot be read as of a moment before,
// or within a few seconds after, the last change to its definition.
var errRedefined = errors.New("the table's definition changed after that moment")

// undoGone recognises "snapshot too old" and its relatives.
func undoGone(err error) bool {
	return hasCode(err, "ORA-01555", "ORA-08180", "ORA-08181", "ORA-30052")
}

// redefined recognises a flashback query refused because of DDL.
func redefined(err error) bool { return hasCode(err, "ORA-01466") }

func hasCode(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range codes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// pastError maps the failures of a flashback query to their sentinels.
func pastError(err error) error {
	switch {
	case undoGone(err):
		return errUndoGone
	case redefined(err):
		return errRedefined
	}
	return oracle.Friendly(err)
}

// settle returns an SCN at which every table can be read with a flashback
// query. Oracle refuses one for the first few seconds after a table's
// definition changed, and switching on key logging is such a change, so
// right after it the first SCNs tried may not do.
func settle(ctx context.Context, src oracle.Queryer, plan *Plan) (int64, error) {
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		scn, err := currentSCN(ctx, src)
		if err != nil {
			return 0, err
		}
		last = nil
		for _, t := range plan.Tables {
			var n int
			err := src.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s AS OF SCN %d WHERE ROWNUM < 1", t.Source(), scn)).Scan(&n)
			if err != nil {
				if !redefined(err) {
					return 0, fmt.Errorf("%s: %w", t.Label(), oracle.Friendly(err))
				}
				last = fmt.Errorf("%s: %w", t.Label(), oracle.Friendly(err))
				break
			}
		}
		if last == nil {
			return scn, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return 0, fmt.Errorf("the tables could not be read as of one moment; their definition keeps changing (%w)", last)
}

// fetchRow reads one row by primary key in the form the copy uses. With
// an SCN it reads the row as it was at that moment. It returns nil when
// the row does not exist.
func fetchRow(ctx context.Context, src oracle.Queryer, t *Table, key []string, scn int64) ([]any, error) {
	asOf := ""
	if scn > 0 {
		asOf = fmt.Sprintf(" AS OF SCN %d", scn)
	}
	args := make([]any, len(key))
	for i, k := range key {
		args[i] = k
	}
	rows, err := src.QueryContext(ctx, "SELECT "+selectList(t)+" FROM "+t.Source()+asOf+" WHERE "+keyWhere(t), args...)
	if err != nil {
		return nil, pastError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, pastError(err)
		}
		return nil, nil
	}
	values := make([]any, len(t.Cols))
	dest := make([]any, len(t.Cols))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, oracle.Friendly(err)
	}
	return values, nil
}
