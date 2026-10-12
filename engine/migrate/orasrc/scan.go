package orasrc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// Besides the redo stream there is a second place where the source
// remembers what changed: its undo data, which a flashback version query
// reads. Asked for the versions a table's rows went through between two
// SCNs, Oracle answers with the rows that committed transactions
// inserted, updated or deleted in between. That is all a job needs to
// know, since every row is read back by its key anyway.
//
// It asks less of the source than LogMiner does: no supplemental logging,
// no archived redo, no privilege beyond the one the copy already needs,
// and it works through a connection to a pluggable database on every
// release. It also sees changes LogMiner cannot describe, such as those
// to a table with a JSON or BOOLEAN column. The price is that each pass
// reads the primary key index of every table it watches, where the redo
// stream holds the changes and nothing else, so it is the second choice:
// used for the tables LogMiner cannot follow, and for all of them when
// LogMiner is not set up, not allowed, or has lost the redo it needed.

// ErrChangesLost reports that neither the redo stream nor the undo data
// reaches back to where the job left off.
var ErrChangesLost = errors.New("the source no longer holds the changes made since this job's last checkpoint: " +
	"its undo data does not reach back that far. Raise UNDO_RETENTION to cover the longest time the job may be paused or behind. " +
	"This job has to start again with a new copy")

// keyText is how a key column is read so that it can be bound again as
// text: the forms KeyWhere takes, which are those the redo stream uses.
func keyText(c Column) string {
	col := oracle.Quote(c.Name)
	switch c.Kind {
	case KindBinary:
		return "RAWTOHEX(" + col + ")"
	case KindNumber, KindDate, KindTimestamp, KindTimestampTZ, KindTimestampLTZ:
		return "TO_CHAR(" + col + ")"
	}
	return col
}

func keyList(t *Table) string {
	exprs := make([]string, len(t.PK))
	for i, col := range t.PK {
		exprs[i] = keyText(t.Cols[col])
	}
	return strings.Join(exprs, ", ")
}

// keys runs a query that returns one primary key per row.
func keys(ctx context.Context, src oracle.Queryer, t *Table, text string) ([][]string, error) {
	rows, err := src.QueryContext(ctx, text)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(t.PK))
		dest := make([]any, len(vals))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		key := make([]string, len(vals))
		for i, v := range vals {
			key[i] = v.String
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// scanned is what one pass over the row versions produced.
type scanned struct {
	ops []RowOp
	// low is the SCN every watched table has been read up to. It is
	// below the end of the pass while a table is waiting to be readable.
	low int64
	// waiting says some table could not be read this time.
	waiting bool
}

// scan asks each table for the rows that changed since it was last
// asked, up to the given SCN. A row whose key changed is reported under
// both keys: the version that ended and the one that began.
func (f *Follower) scan(ctx context.Context, src *sql.Conn, tables []*Table, to int64) (*scanned, error) {
	out := &scanned{low: to}
	for _, t := range tables {
		label := t.Label()
		from, ok := f.bounds[label]
		if !ok {
			from = f.base
		}
		if from >= to {
			continue
		}
		// The lower bound is read again on purpose: applying a row twice
		// does no harm, and no commit can fall between two passes.
		list, err := keys(ctx, src, t, fmt.Sprintf(`SELECT DISTINCT %s FROM %s VERSIONS BETWEEN SCN %d AND %d
			WHERE versions_startscn IS NOT NULL OR versions_endscn IS NOT NULL`, keyList(t), t.Source(), from, to))
		switch {
		case err == nil:
			for _, key := range list {
				out.ops = append(out.ops, RowOp{Table: t, Key: key})
			}
			f.bounds[label] = to
		case Redefined(err):
			// The table was truncated or altered, and its versions from
			// before that cannot be told from those after. All of its
			// rows are brought over again, as soon as it can be read as
			// of one moment, which Oracle refuses for the first seconds
			// after the change.
			at, list, rerr := f.reread(ctx, src, t)
			if rerr != nil {
				return nil, rerr
			}
			if at == 0 {
				out.low, out.waiting = min(out.low, from), true
				continue
			}
			out.ops = append(out.ops, RowOp{Table: t, Truncate: true})
			for _, key := range list {
				out.ops = append(out.ops, RowOp{Table: t, Key: key})
			}
			f.bounds[label] = at
			out.low = min(out.low, at)
			f.Reporter.Event("warn", fmt.Sprintf("%s was truncated or its definition changed on the source; its %d row(s) are brought over again. The target table keeps the shape it has", label, len(list)))
		case UndoGone(err):
			return nil, fmt.Errorf("%s: %w", label, ErrChangesLost)
		default:
			return nil, fmt.Errorf("read the changed rows of %s: %w", label, oracle.Friendly(err))
		}
	}
	return out, nil
}

// reread lists every key a table holds at a moment from which its
// versions can be followed again, and returns that moment. It returns 0
// when the table cannot be read that way yet.
func (f *Follower) reread(ctx context.Context, src *sql.Conn, t *Table) (int64, [][]string, error) {
	at, err := CurrentSCN(ctx, src)
	if err != nil {
		return 0, nil, err
	}
	list, err := keys(ctx, src, t, fmt.Sprintf("SELECT %s FROM %s AS OF SCN %d", keyList(t), t.Source(), at))
	if Redefined(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read %s again: %w", t.Label(), oracle.Friendly(err))
	}
	return at, list, nil
}

// unmined reports whether LogMiner cannot describe changes to a table:
// those with a JSON or BOOLEAN column come back as operations it does
// not support.
func unmined(t *Table) bool {
	for _, c := range t.Cols {
		if c.Kind == KindJSON || c.Kind == KindBool {
			return true
		}
	}
	return false
}

// MiningBlocked says why changes cannot be read with LogMiner through
// this connection, or returns "" when they can: minimal supplemental
// logging has to be on, and the account, the release and the place the
// connection leads to have to allow a LogMiner session.
func MiningBlocked(ctx context.Context, src *sql.DB) string {
	var minimal int
	_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM v$database WHERE supplemental_log_data_min IN ('YES', 'IMPLICIT')`).Scan(&minimal)
	if minimal == 0 {
		_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM dba_supplemental_logging WHERE minimal = 'YES'`).Scan(&minimal)
	}
	if minimal == 0 {
		return "minimal supplemental logging is not switched on"
	}
	if err := trialMine(ctx, src); err != nil {
		return err.Error()
	}
	return ""
}
