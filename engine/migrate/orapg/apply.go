package orapg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/migrate/orasrc"
)

// applyChunk is how many row changes go into one target transaction.
const applyChunk = 5000

// follower sets up the following of changes, with this job's target and
// state at the receiving end.
func (r *runner) follower() *orasrc.Follower {
	return &orasrc.Follower{
		Source: r.src, Catalog: r.plan.Catalog, Target: r, Reporter: r.rep,
		Advanced: func(scn, lagSeconds int64) {
			r.st.AppliedLSN = fmt.Sprintf("SCN %d", scn)
			r.st.LagSeconds = lagSeconds
		},
		Applied: func(transactions, rows int64) {
			r.st.TxApplied += transactions
			r.st.RowsApplied += rows
			r.st.LastCommitTime = time.Now().UTC()
		},
		Anomaly: r.anomaly,
		Save:    r.save,
	}
}

// Checkpoint returns the SCN up to which changes have been applied to
// the target, or 0 when none have.
func (r *runner) Checkpoint(ctx context.Context) (int64, error) {
	if _, err := r.dst.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return 0, errors.New(pgMessage(err))
	}
	// The checkpoint on the target is the truth: it was committed together
	// with the changes it covers.
	var scn int64
	err := r.dst.QueryRow(ctx, `SELECT scn FROM `+metaSchema+`.oracle_checkpoint WHERE job_id = $1`, r.jobID).Scan(&scn)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	return scn, nil
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
		if t.Cols[t.PK[i]].Kind == orasrc.KindBinary {
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

// Apply brings the rows of the committed transactions in line with the
// source and moves the checkpoint, in one target transaction per chunk.
func (r *runner) Apply(ctx context.Context, ops []orasrc.RowOp, fetch orasrc.Fetch, checkpoint int64) error {
	var stmts []*statement
	for _, op := range ops {
		t := r.plan.of(op.Table)
		if op.Truncate {
			stmts = append(stmts, &statement{sql: "TRUNCATE " + t.QName()})
			continue
		}
		del := deleteStmt(t, op.Key)
		row, err := fetch(ctx, op.Table, op.Key)
		if err != nil {
			return err
		}
		if row == nil {
			// The source does not have the row: it was deleted, or its
			// key changed. Make sure it is gone here as well.
			stmts = append(stmts, del)
			continue
		}
		args := make([][]byte, len(row))
		for i, v := range row {
			if s, ok := pgText(t.Cols[i], v); ok {
				args[i] = []byte(s)
			}
		}
		stmts = append(stmts, &statement{sql: upsertSQL(t), args: args, insert: true, del: del})
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
