package orasrc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// RowOp is one row to bring in line with the source: it is read again by
// its key, then stored, or removed if the source no longer has it. An
// insert, an update and a delete are all the same instruction, which is
// why a change undone by a rollback to a savepoint needs no special
// care. A truncate empties the whole table.
type RowOp struct {
	Table *Table
	// Key is the row's primary key in the text form the redo stream
	// carries: the session's formats for numbers and dates, hexadecimal
	// digits for RAW.
	Key      []string
	Truncate bool
}

// openTx collects the rows a source transaction has touched so far. They
// are applied when its commit is seen and dropped if it rolls back.
type openTx struct {
	first int64 // SCN of its first change
	ops   []RowOp
}

// mined is what one pass over the redo stream produced.
type mined struct {
	ops        []RowOp // rows of the transactions that committed, in commit order
	commits    int64
	lastCommit int64 // SCN of the last of those commits
	lagSeconds int64
}

// mine reads the redo between two SCNs and sorts what it finds into the
// transactions it belongs to. Transactions that committed in the range
// are returned; those still running stay in open for the next pass.
func (f *Follower) mine(ctx context.Context, miner *sql.Conn, from, to int64, open map[string]*openTx) (*mined, error) {
	if err := startMiner(ctx, miner, from, to); err != nil {
		return nil, err
	}
	defer miner.ExecContext(context.WithoutCancel(ctx), `BEGIN DBMS_LOGMNR.END_LOGMNR; END;`)

	rows, err := miner.QueryContext(ctx, fmt.Sprintf(`SELECT scn, operation_code, RAWTOHEX(xid), seg_owner, table_name, csf, rollback, sql_redo,
			ROUND((SYSDATE - timestamp) * 86400)
		FROM v$logmnr_contents
		WHERE scn BETWEEN %d AND %d AND (operation_code IN (%d, %d)
		   OR (operation_code IN (%d, %d, %d, %d, %d, 255) AND seg_owner IN (%s)))`,
		from, to, OpCommit, OpRollback, OpInsert, OpDelete, OpUpdate, OpDDL, OpLobLocator, f.Catalog.ownerList()))
	if err != nil {
		return nil, redoError(err)
	}
	defer rows.Close()

	out := &mined{}
	var pending strings.Builder // a statement continued over several rows
	for rows.Next() {
		var scn, age int64
		var op, csf, undone int
		var xid string
		var owner, table, text sql.NullString
		if err := rows.Scan(&scn, &op, &xid, &owner, &table, &csf, &undone, &text, &age); err != nil {
			return nil, redoError(err)
		}
		switch op {
		case OpCommit:
			if tx := open[xid]; tx != nil {
				out.ops = append(out.ops, tx.ops...)
				out.commits++
				out.lastCommit = scn
				out.lagSeconds = max(age, 0)
				delete(open, xid)
			}
			continue
		case OpRollback:
			delete(open, xid)
			continue
		}
		pending.WriteString(text.String)
		if csf == 1 {
			continue
		}
		stmt := pending.String()
		pending.Reset()
		t := f.Catalog.Table(owner.String, table.String)
		if t == nil || (len(t.PK) > 0 && unmined(t)) {
			// Not in scope, or followed through its row versions.
			continue
		}
		switch {
		case undone == 1:
			// The undoing of an earlier change in the same transaction,
			// after a rollback to a savepoint. It names its row by
			// address only, but the change it undoes already put that
			// row on the list, and the row is read as it stands at commit.
			continue
		case op == OpDDL:
			// DDL commits by itself, so it takes effect where it stands.
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(stmt)), "truncate") {
				out.ops = append(out.ops, RowOp{Table: t, Truncate: true})
				out.commits++
				out.lastCommit = scn
			} else if !strings.Contains(strings.ToUpper(stmt), "SUPPLEMENTAL LOG") {
				// Switching key logging on or off, as a job does itself,
				// changes nothing about the table's shape.
				f.Anomaly(fmt.Sprintf("the source changed the definition of %s (%s); the target table was left as it is", t.Label(), brief(stmt)))
			}
			continue
		case op == 255:
			f.Anomaly(fmt.Sprintf("LogMiner could not describe a change to %s; that row may differ on the target", t.Label()))
			continue
		case len(t.PK) == 0:
			if !f.keyless[t.Label()] {
				if f.keyless == nil {
					f.keyless = map[string]bool{}
				}
				f.keyless[t.Label()] = true
				f.Anomaly(fmt.Sprintf("%s has no primary key, so the changes made to it after the copy are not followed", t.Label()))
			}
			continue
		}
		parsed, err := ParseRedo(op, stmt)
		if err != nil {
			f.Anomaly(fmt.Sprintf("skipped a change to %s that could not be read: %s", t.Label(), err))
			continue
		}
		gone, now, err := parsed.Keys(t, op)
		if err != nil {
			f.Anomaly(err.Error())
			continue
		}
		tx := open[xid]
		if tx == nil {
			tx = &openTx{first: scn}
			open[xid] = tx
		}
		if gone != nil {
			tx.ops = append(tx.ops, RowOp{Table: t, Key: gone})
		}
		if now != nil {
			tx.ops = append(tx.ops, RowOp{Table: t, Key: now})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, redoError(err)
	}
	return out, nil
}

// startMiner opens a LogMiner session over a range of SCNs. The online
// catalog is used as the dictionary, so table and column names are those
// the source has now.
//
// Inside a pluggable database of a current release LogMiner finds the
// redo logs for the range by itself. Everywhere else (a database that is
// not a container, the container root, Oracle 12c) it has to be told
// which files to read, so when it reports that none were given, the
// online and archived logs that cover the range are registered and the
// session is started again.
func startMiner(ctx context.Context, miner *sql.Conn, from, to int64) error {
	start := fmt.Sprintf(`BEGIN DBMS_LOGMNR.START_LOGMNR(STARTSCN => %d, ENDSCN => %d, OPTIONS => DBMS_LOGMNR.DICT_FROM_ONLINE_CATALOG); END;`, from, to)
	_, err := miner.ExecContext(ctx, start)
	if HasCode(err, "ORA-65040") {
		return fmt.Errorf("%w (%s)", errNoMiner, oracle.Friendly(err))
	}
	if err == nil || !HasCode(err, "ORA-01291", "ORA-01292") {
		return redoError(err)
	}
	files, ferr := logFiles(ctx, miner, from, to)
	if ferr != nil || len(files) == 0 {
		return redoError(err)
	}
	for i, name := range files {
		option := "DBMS_LOGMNR.ADDFILE"
		if i == 0 {
			option = "DBMS_LOGMNR.NEW"
		}
		if _, aerr := miner.ExecContext(ctx, `BEGIN DBMS_LOGMNR.ADD_LOGFILE(LOGFILENAME => :1, OPTIONS => `+option+`); END;`, name); aerr != nil {
			if HasCode(aerr, "ORA-65040") {
				// Inside a pluggable database log files cannot be
				// named, and LogMiner has just said that it cannot
				// find the range by itself: the redo is out of reach.
				return redoError(err)
			}
			return fmt.Errorf("register redo log %s with LogMiner: %w", name, oracle.Friendly(aerr))
		}
	}
	_, err = miner.ExecContext(ctx, start)
	return redoError(err)
}

// logFiles lists the redo logs that hold the changes between two SCNs:
// the archived copy of each log where there is one, the online log
// otherwise.
func logFiles(ctx context.Context, src oracle.Queryer, from, to int64) ([]string, error) {
	rows, err := query(ctx, src, fmt.Sprintf(`SELECT name FROM (
			SELECT MIN(a.name) name, a.thread#, a.sequence# FROM v$archived_log a
			 WHERE a.name IS NOT NULL AND a.deleted = 'NO' AND a.standby_dest = 'NO'
			   AND a.next_change# > %[1]d AND a.first_change# <= %[2]d
			 GROUP BY a.thread#, a.sequence#
			UNION ALL
			SELECT MIN(f.member), l.thread#, l.sequence# FROM v$log l JOIN v$logfile f ON f.group# = l.group#
			 WHERE l.status <> 'UNUSED' AND l.next_change# > %[1]d AND l.first_change# <= %[2]d
			   AND NOT EXISTS (SELECT 1 FROM v$archived_log a WHERE a.thread# = l.thread# AND a.sequence# = l.sequence#
			                   AND a.name IS NOT NULL AND a.deleted = 'NO' AND a.standby_dest = 'NO')
			 GROUP BY l.thread#, l.sequence#
		) ORDER BY thread#, sequence#`, from, to))
	if err != nil {
		return nil, err
	}
	files := make([]string, len(rows))
	for i, r := range rows {
		files[i] = r[0]
	}
	return files, nil
}

// brief shortens a statement for a message.
func brief(stmt string) string {
	stmt = strings.Join(strings.Fields(stmt), " ")
	if len(stmt) > 120 {
		stmt = stmt[:120] + "..."
	}
	return stmt
}

// errRedoGone reports that the redo a job still had to read is no longer
// within LogMiner's reach. A database in NOARCHIVELOG mode keeps only its
// online redo logs, and from inside a pluggable database LogMiner reads
// only the current one, so a log switch is enough.
var errRedoGone = errors.New("the redo this job must read next can no longer be read on the source")

// errNoMiner reports that LogMiner cannot be run through this connection
// at all, as on Oracle 12c from inside a pluggable database.
var errNoMiner = errors.New("this Oracle release cannot run LogMiner from inside a pluggable database")

// redoError tells the failures of a LogMiner session apart.
func redoError(err error) error {
	if err == nil {
		return nil
	}
	if HasCode(err, "ORA-01291", "ORA-01292", "ORA-01284", "ORA-01289", "ORA-01281") {
		return fmt.Errorf("%w (%s)", errRedoGone, oracle.Friendly(err))
	}
	return oracle.Friendly(err)
}

// Coalesce keeps the last thing said about each row. Every operation
// means "make the target's row equal to the source's", so one per row is
// enough, and a truncate makes what came before it for that table moot.
func Coalesce(ops []RowOp) []RowOp {
	last := map[string]int{}
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.Truncate {
			for j := 0; j < i; j++ {
				if keep[j] && ops[j].Table == op.Table {
					keep[j] = false
				}
			}
			keep[i] = true
			continue
		}
		id := op.Table.Label() + "\x00" + strings.Join(op.Key, "\x00")
		if j, ok := last[id]; ok {
			keep[j] = false
		}
		last[id], keep[i] = i, true
	}
	out := ops[:0:0]
	for i, op := range ops {
		if keep[i] {
			out = append(out, op)
		}
	}
	return out
}
