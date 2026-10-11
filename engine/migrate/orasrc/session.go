package orasrc

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

// Pinned takes one connection out of the pool and sets its formats.
func Pinned(ctx context.Context, src *sql.DB) (*sql.Conn, error) {
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

// CurrentSCN reads the database's system change number. V$DATABASE is
// the usual place; an account without access to it may still be allowed
// to ask DBMS_FLASHBACK.
func CurrentSCN(ctx context.Context, src oracle.Queryer) (int64, error) {
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

// OldestOpen returns the SCN at which the oldest transaction still open
// began, or 0 when there is none or the account cannot see them.
func OldestOpen(ctx context.Context, src oracle.Queryer) int64 {
	var scn sql.NullInt64
	_ = src.QueryRowContext(ctx, `SELECT MIN(start_scn) FROM v$transaction WHERE start_scn > 0`).Scan(&scn)
	return scn.Int64
}

// KeyWhere is the condition that selects one row by primary key, with
// the key bound as text. Fixed-width and binary keys need a conversion
// to compare equal.
func KeyWhere(t *Table) string {
	conds := make([]string, len(t.PK))
	for i, col := range t.PK {
		c := t.Cols[col]
		bind := fmt.Sprintf(":%d", i+1)
		switch {
		case c.Kind == KindBinary:
			bind = "HEXTORAW(" + bind + ")"
		case c.DataType == "CHAR" || c.DataType == "NCHAR":
			bind = fmt.Sprintf("CAST(%s AS %s(%d))", bind, c.DataType, max(c.CharLength, 1))
		}
		conds[i] = oracle.Quote(c.Name) + " = " + bind
	}
	return strings.Join(conds, " AND ")
}

// ErrUndoGone reports that Oracle no longer keeps the undo needed to
// read the source as of a past moment.
var ErrUndoGone = errors.New("the source no longer holds the undo data for that moment")

// ErrRedefined reports that a table cannot be read as of a moment before,
// or within a few seconds after, the last change to its definition.
var ErrRedefined = errors.New("the table's definition changed after that moment")

// UndoGone recognises "snapshot too old" and its relatives.
func UndoGone(err error) bool {
	return HasCode(err, "ORA-01555", "ORA-08180", "ORA-08181", "ORA-30052")
}

// Redefined recognises a flashback query refused because of DDL.
func Redefined(err error) bool { return HasCode(err, "ORA-01466") }

// HasCode reports whether an error carries one of the given ORA- codes.
func HasCode(err error, codes ...string) bool {
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
	case UndoGone(err):
		return ErrUndoGone
	case Redefined(err):
		return ErrRedefined
	}
	return oracle.Friendly(err)
}

// Settle returns an SCN at which every table can be read with a flashback
// query. Oracle refuses one for the first few seconds after a table's
// definition changed, and switching on key logging is such a change, so
// right after it the first SCNs tried may not do.
func Settle(ctx context.Context, src oracle.Queryer, cat *Catalog) (int64, error) {
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		scn, err := CurrentSCN(ctx, src)
		if err != nil {
			return 0, err
		}
		last = nil
		for _, t := range cat.Tables {
			var n int
			err := src.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s AS OF SCN %d WHERE ROWNUM < 1", t.Source(), scn)).Scan(&n)
			if err != nil {
				if !Redefined(err) {
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

// FetchRow reads one row by primary key in the form the copy uses. With
// an SCN it reads the row as it was at that moment. It returns nil when
// the row does not exist.
func FetchRow(ctx context.Context, src oracle.Queryer, t *Table, key []string, scn int64) ([]any, error) {
	asOf := ""
	if scn > 0 {
		asOf = fmt.Sprintf(" AS OF SCN %d", scn)
	}
	args := make([]any, len(key))
	for i, k := range key {
		args[i] = k
	}
	rows, err := src.QueryContext(ctx, "SELECT "+SelectList(t)+" FROM "+t.Source()+asOf+" WHERE "+KeyWhere(t), args...)
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

// StartPosition picks the moment the copy reads the source at and the
// SCN the redo stream is first read from.
//
// The redo stream is read from before the snapshot, far enough back
// to include the first change of every transaction still open when
// the snapshot is taken. The order of these three reads is what
// guarantees it: a transaction open at the snapshot either began
// before the first read, and is then seen by the second, or began
// after it.
func StartPosition(ctx context.Context, src oracle.Queryer, cat *Catalog) (snapshot, mineFrom int64, err error) {
	before, err := CurrentSCN(ctx, src)
	if err != nil {
		return 0, 0, err
	}
	oldest := OldestOpen(ctx, src)
	snapshot, err = Settle(ctx, src, cat)
	if err != nil {
		return 0, 0, err
	}
	mineFrom = before
	if oldest > 0 && oldest < before {
		mineFrom = oldest
	}
	return snapshot, mineFrom, nil
}
