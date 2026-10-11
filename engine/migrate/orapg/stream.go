package orapg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector/oracle"
)

// Reasons the stream stops without an error.
const (
	stopPause   = "pause"
	stopAbort   = "abort"
	stopCutover = "cutover"
)

// applyChunk is how many row changes go into one target transaction.
const applyChunk = 5000

// rowOp is one row to bring in line with the source: it is read again by
// its key, then stored, or removed if the source no longer has it. An
// insert, an update and a delete are all the same instruction, which is
// why a change undone by a rollback to a savepoint needs no special
// care. A truncate empties the whole table.
type rowOp struct {
	t        *Table
	key      []string
	truncate bool
}

// openTx collects the rows a source transaction has touched so far. They
// are applied when its commit is seen and dropped if it rolls back.
type openTx struct {
	first int64 // SCN of its first change
	ops   []rowOp
}

// mined is what one pass over the redo stream produced.
type mined struct {
	ops        []rowOp // rows of the transactions that committed, in commit order
	commits    int64
	lastCommit int64 // SCN of the last of those commits
	lagSeconds int64
}

// stream follows the source's redo with LogMiner and applies what it
// finds until the operator pauses, aborts or cuts over. It returns why
// it stopped.
func (r *runner) stream(ctx context.Context) (string, error) {
	if _, err := r.dst.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return "", errors.New(pgMessage(err))
	}
	// The checkpoint on the target is the truth: it was committed together
	// with the changes it covers.
	var scn int64
	err := r.dst.QueryRow(ctx, `SELECT scn FROM `+metaSchema+`.oracle_checkpoint WHERE job_id = $1`, r.jobID).Scan(&scn)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if scn == 0 {
		// Nothing applied yet: start where the copy says to.
		scn = r.st.MineFrom - 1
	}

	// LogMiner belongs to a session, so mining keeps one connection for
	// itself and rows are read back on another.
	miner, err := pinned(ctx, r.src)
	if err != nil {
		return "", err
	}
	defer miner.Close()
	reader, err := pinned(ctx, r.src)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	open := map[string]*openTx{}
	draining, quiet, sinceCheckpoint := false, 0, 0
	for {
		cur, err := currentSCN(ctx, miner)
		if err != nil {
			return "", err
		}
		if cur > scn {
			m, err := r.mine(ctx, miner, scn+1, cur, open)
			if err != nil {
				return "", err
			}
			// A restart must read again from the first change of the
			// oldest transaction that has not ended yet.
			checkpoint := cur
			for _, tx := range open {
				if tx.first-1 < checkpoint {
					checkpoint = tx.first - 1
				}
			}
			sinceCheckpoint++
			if len(m.ops) > 0 || sinceCheckpoint >= 10 {
				if err := r.apply(ctx, reader, m, checkpoint); err != nil {
					return "", err
				}
				sinceCheckpoint = 0
			}
			scn = cur
			r.st.AppliedLSN = fmt.Sprintf("SCN %d", scn)
			r.st.LagSeconds = m.lagSeconds
			if len(m.ops) == 0 && len(open) == 0 {
				quiet++
				r.st.LagSeconds = 0
			} else {
				quiet = 0
			}
		}
		r.save()

		switch r.rep.Desired() {
		case "pause":
			return stopPause, nil
		case "abort":
			return stopAbort, nil
		case "cutover":
			if !draining {
				draining, quiet = true, 0
				r.rep.Event("info", "cutover requested: applying the remaining changes")
			}
		}
		// Two passes in a row that found nothing to apply and no
		// transaction in flight mean the source has gone quiet.
		if draining && quiet >= 2 {
			return stopCutover, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// mine reads the redo between two SCNs and sorts what it finds into the
// transactions it belongs to. Transactions that committed in the range
// are returned; those still running stay in open for the next pass.
func (r *runner) mine(ctx context.Context, miner *sql.Conn, from, to int64, open map[string]*openTx) (*mined, error) {
	if err := startMiner(ctx, miner, from, to); err != nil {
		return nil, err
	}
	defer miner.ExecContext(context.WithoutCancel(ctx), `BEGIN DBMS_LOGMNR.END_LOGMNR; END;`)

	owners := make([]string, len(r.plan.Owners))
	for i, o := range r.plan.Owners {
		owners[i] = literal(o)
	}
	rows, err := miner.QueryContext(ctx, fmt.Sprintf(`SELECT scn, operation_code, RAWTOHEX(xid), seg_owner, table_name, csf, rollback, sql_redo,
			ROUND((SYSDATE - timestamp) * 86400)
		FROM v$logmnr_contents
		WHERE scn BETWEEN %d AND %d AND (operation_code IN (%d, %d)
		   OR (operation_code IN (%d, %d, %d, %d, %d, 255) AND seg_owner IN (%s)))`,
		from, to, opCommit, opRollback, opInsert, opDelete, opUpdate, opDDL, opLobLocator, strings.Join(owners, ", ")))
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
		case opCommit:
			if tx := open[xid]; tx != nil {
				out.ops = append(out.ops, tx.ops...)
				out.commits++
				out.lastCommit = scn
				out.lagSeconds = max(age, 0)
				delete(open, xid)
			}
			continue
		case opRollback:
			delete(open, xid)
			continue
		}
		pending.WriteString(text.String)
		if csf == 1 {
			continue
		}
		stmt := pending.String()
		pending.Reset()
		t := r.plan.Table(owner.String, table.String)
		if t == nil {
			continue
		}
		switch {
		case undone == 1:
			// The undoing of an earlier change in the same transaction,
			// after a rollback to a savepoint. It names its row by
			// address only, but the change it undoes already put that
			// row on the list, and the row is read as it stands at commit.
			continue
		case op == opDDL:
			// DDL commits by itself, so it takes effect where it stands.
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(stmt)), "truncate") {
				out.ops = append(out.ops, rowOp{t: t, truncate: true})
				out.commits++
				out.lastCommit = scn
			} else if !strings.Contains(strings.ToUpper(stmt), "SUPPLEMENTAL LOG") {
				// Switching key logging on or off, as this job does
				// itself, changes nothing about the table's shape.
				r.anomaly(fmt.Sprintf("the source changed the definition of %s (%s); the target table was left as it is", t.Label(), brief(stmt)))
			}
			continue
		case op == 255:
			r.anomaly(fmt.Sprintf("LogMiner could not describe a change to %s; that row may differ on the target", t.Label()))
			continue
		case len(t.PK) == 0:
			if !r.keyless[t.Label()] {
				r.keyless[t.Label()] = true
				r.anomaly(fmt.Sprintf("%s has no primary key, so the changes made to it after the copy are not followed", t.Label()))
			}
			continue
		}
		parsed, err := parseRedo(op, stmt)
		if err != nil {
			r.anomaly(fmt.Sprintf("skipped a change to %s that could not be read: %s", t.Label(), err))
			continue
		}
		gone, now, err := parsed.keys(t, op)
		if err != nil {
			r.anomaly(err.Error())
			continue
		}
		tx := open[xid]
		if tx == nil {
			tx = &openTx{first: scn}
			open[xid] = tx
		}
		if gone != nil {
			tx.ops = append(tx.ops, rowOp{t: t, key: gone})
		}
		if now != nil {
			tx.ops = append(tx.ops, rowOp{t: t, key: now})
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
	if err == nil || !hasCode(err, "ORA-01291", "ORA-01292") {
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
			if hasCode(aerr, "ORA-65040") {
				return errors.New("this Oracle release cannot run LogMiner from inside a pluggable database, so changes cannot be followed through this connection. Use an offline migration for it")
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

// redoError explains the failure that ends a job for good: the redo it
// still had to read is no longer on the source.
func redoError(err error) error {
	if err == nil {
		return nil
	}
	if hasCode(err, "ORA-01291", "ORA-01292", "ORA-01284", "ORA-01289", "ORA-01281") {
		return fmt.Errorf("the source no longer has the redo log this job must read next (%s). "+
			"A database in NOARCHIVELOG mode keeps only its online logs, which are reused once full; "+
			"run the source in ARCHIVELOG mode to follow changes dependably. This job has to start again with a new copy", oracle.Friendly(err))
	}
	return oracle.Friendly(err)
}

// coalesce keeps the last thing said about each row. Every operation
// means "make the target's row equal to the source's", so one per row is
// enough, and a truncate makes what came before it for that table moot.
func coalesce(ops []rowOp) []rowOp {
	last := map[string]int{}
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.truncate {
			for j := 0; j < i; j++ {
				if keep[j] && ops[j].t == op.t {
					keep[j] = false
				}
			}
			keep[i] = true
			continue
		}
		id := op.t.Label() + "\x00" + strings.Join(op.key, "\x00")
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

// statement is one prepared change for the target.
type statement struct {
	sql  string
	args [][]byte
	// insert is set for a row to store; it is what a retry re-inserts
	// after deleting every row of the group.
	insert bool
	del    *statement
}

// pgKey converts a key read from the redo stream to PostgreSQL's text
// form for the key columns.
func pgKey(t *Table, key []string) [][]byte {
	out := make([][]byte, len(key))
	for i, k := range key {
		if t.Cols[t.PK[i]].Kind == kBinary {
			k = `\x` + k
		}
		out[i] = []byte(k)
	}
	return out
}

func deleteStmt(t *Table, key []string) *statement {
	conds := make([]string, len(t.PK))
	for i, col := range t.PK {
		conds[i] = fmt.Sprintf("%s = $%d", qident(t.Cols[col].Target), i+1)
	}
	return &statement{sql: "DELETE FROM " + t.QName() + " WHERE " + strings.Join(conds, " AND "), args: pgKey(t, key)}
}

func upsertSQL(t *Table) string {
	cols := make([]string, len(t.Cols))
	params := make([]string, len(t.Cols))
	var sets, pk []string
	isKey := map[int]bool{}
	for _, i := range t.PK {
		isKey[i] = true
		pk = append(pk, qident(t.Cols[i].Target))
	}
	for i, c := range t.Cols {
		cols[i], params[i] = qident(c.Target), fmt.Sprintf("$%d", i+1)
		if !isKey[i] {
			sets = append(sets, cols[i]+" = EXCLUDED."+cols[i])
		}
	}
	action := "DO NOTHING"
	if len(sets) > 0 {
		action = "DO UPDATE SET " + strings.Join(sets, ", ")
	}
	return "INSERT INTO " + t.QName() + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(params, ", ") +
		") ON CONFLICT (" + strings.Join(pk, ", ") + ") " + action
}

// apply brings the rows of the committed transactions in line with the
// source and moves the checkpoint, in one target transaction per chunk.
func (r *runner) apply(ctx context.Context, reader *sql.Conn, m *mined, checkpoint int64) error {
	ops := coalesce(m.ops)
	var stmts []*statement
	for _, op := range ops {
		if op.truncate {
			stmts = append(stmts, &statement{sql: "TRUNCATE " + op.t.QName()})
			continue
		}
		del := deleteStmt(op.t, op.key)
		// Read the row as it stood when the last of these transactions
		// committed, so the target passes through states the source was
		// really in. If that moment cannot be read any more, the row as it is
		// now is the next best thing: later changes are still to come.
		row, err := fetchRow(ctx, reader, op.t, op.key, m.lastCommit)
		if errors.Is(err, errUndoGone) || errors.Is(err, errRedefined) {
			if errors.Is(err, errUndoGone) && !r.undoWarned {
				r.undoWarned = true
				r.rep.Event("warn", "the source no longer keeps undo data back to the changes being applied; rows are read as they are now instead. Raise UNDO_RETENTION if this job often runs behind")
			}
			row, err = fetchRow(ctx, reader, op.t, op.key, 0)
		}
		if err != nil {
			return fmt.Errorf("read %s back from the source: %w", op.t.Label(), err)
		}
		if row == nil {
			// The source does not have the row: it was deleted, or its
			// key changed. Make sure it is gone here as well.
			stmts = append(stmts, del)
			continue
		}
		args := make([][]byte, len(row))
		for i, v := range row {
			if s, ok := pgText(op.t.Cols[i], v); ok {
				args[i] = []byte(s)
			}
		}
		stmts = append(stmts, &statement{sql: upsertSQL(op.t), args: args, insert: true, del: del})
	}

	for start := 0; start == 0 || start < len(stmts); start += applyChunk {
		end := min(start+applyChunk, len(stmts))
		chunk := stmts[start:end]
		// Only the last chunk covers everything up to the checkpoint.
		move := end == len(stmts)
		err := r.send(ctx, chunk, checkpoint, move)
		if pgCode(err) == "23505" {
			// Rows were stored one by one, and a unique value passed from
			// one row to another within the group collided on the way.
			// Removing the group's rows first makes the order irrelevant.
			var retry []*statement
			for _, s := range chunk {
				if s.insert {
					retry = append(retry, s.del)
				}
			}
			err = r.send(ctx, append(retry, chunk...), checkpoint, move)
		}
		if err != nil {
			return errors.New(pgMessage(err))
		}
	}
	if len(ops) > 0 {
		r.st.TxApplied += m.commits
		r.st.RowsApplied += int64(len(ops))
		r.st.LastCommitTime = time.Now().UTC()
	}
	return nil
}

// send runs statements in one transaction on the target.
func (r *runner) send(ctx context.Context, stmts []*statement, checkpoint int64, move bool) error {
	pg := r.dst.PgConn()
	b := &pgconn.Batch{}
	b.ExecParams("BEGIN", nil, nil, nil, nil)
	for _, s := range stmts {
		b.ExecParams(s.sql, s.args, nil, nil, nil)
	}
	if move {
		b.ExecParams(checkpointUpsert, [][]byte{[]byte(r.jobID), []byte(fmt.Sprint(checkpoint))}, nil, nil, nil)
	}
	b.ExecParams("COMMIT", nil, nil, nil, nil)
	if _, err := pg.ExecBatch(ctx, b).ReadAll(); err != nil {
		pg.Exec(context.WithoutCancel(ctx), "ROLLBACK").ReadAll()
		return err
	}
	return nil
}

func (r *runner) anomaly(msg string) {
	r.st.Anomalies++
	if r.st.Anomalies <= 20 {
		r.rep.Event("warn", msg)
	}
}
