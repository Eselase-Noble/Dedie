package oramy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate/orasrc"
)

// applyChunk is how many row changes go into one target transaction.
const applyChunk = 5000

// duplicateEntry is MySQL's error for a value a unique index already has.
const duplicateEntry = 1062

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
	if r.applier == nil {
		// A pool of its own, so that the settings of applierParams never
		// reach a statement that needs foreign keys checked. One
		// connection is all it uses.
		db, err := mysql.Open(ctx, r.dstCfg, applierParams)
		if err != nil {
			return 0, fmt.Errorf("target: %w", err)
		}
		db.SetMaxOpenConns(1)
		r.applier = db
	}
	if _, err := r.applier.ExecContext(ctx, checkpointDDL); err != nil {
		return 0, errors.New(message(err))
	}
	// The checkpoint on the target is the truth: it was committed together
	// with the changes it covers.
	var scn int64
	err := r.applier.QueryRowContext(ctx, "SELECT scn FROM `"+checkpointTable+"` WHERE job_id = ?", r.jobID).Scan(&scn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New(message(err))
	}
	return scn, nil
}

// group is what one chunk of changes comes to for one table.
type group struct {
	t *Table
	// keys are the rows to remove: every row the chunk touches.
	keys [][]any
	// rows are those the source still has, to be stored again whole.
	rows [][]any
}

// changes is one chunk of row operations, read back from the source and
// sorted by table.
type changes struct {
	truncates []*Table
	groups    []*group
	byTable   map[*Table]*group
}

func (c *changes) group(t *Table) *group {
	g := c.byTable[t]
	if g == nil {
		g = &group{t: t}
		c.byTable[t] = g
		c.groups = append(c.groups, g)
	}
	return g
}

// key converts a key read from the redo stream to the values MySQL
// compares the key columns with.
func key(t *Table, k []string) []any {
	out := make([]any, len(k))
	for i, v := range k {
		out[i] = myKey(t.Cols[t.PK[i]], v)
	}
	return out
}

// read fetches the rows a chunk of operations names and sorts the work
// by table. fetch may be nil when only the keys are wanted.
func (r *runner) read(ctx context.Context, ops []orasrc.RowOp, fetch orasrc.Fetch) (*changes, error) {
	c := &changes{byTable: map[*Table]*group{}}
	for _, op := range ops {
		t := r.plan.of(op.Table)
		if op.Truncate {
			c.truncates = append(c.truncates, t)
			continue
		}
		g := c.group(t)
		g.keys = append(g.keys, key(t, op.Key))
		if fetch == nil {
			continue
		}
		row, err := fetch(ctx, op.Table, op.Key)
		if err != nil {
			return nil, err
		}
		// Without a row the source does not have it any more: it was
		// deleted, or its key changed. Removing it is all there is to do.
		if row == nil {
			continue
		}
		for i, v := range row {
			row[i] = myValue(t.Cols[i], v)
		}
		g.rows = append(g.rows, row)
	}
	return c, nil
}

// Apply brings the rows of the committed transactions in line with the
// source and moves the checkpoint, in one target transaction per chunk.
//
// A row is stored by deleting it and inserting it again, all deletes of
// a chunk before its first insert. MySQL's INSERT ... ON DUPLICATE KEY
// UPDATE would be one statement instead of two, but it treats a clash on
// any unique index as "this row exists" and silently overwrites whichever
// row holds the value. When two rows swap a unique value, that row is the
// wrong one. With the rows of the chunk out of the way first, the order
// they are stored in does not matter, and a clash that remains is a real
// error.
func (r *runner) Apply(ctx context.Context, ops []orasrc.RowOp, fetch orasrc.Fetch, checkpoint int64) error {
	cleared := false
	for start := 0; start == 0 || start < len(ops); start += applyChunk {
		end := min(start+applyChunk, len(ops))
		c, err := r.read(ctx, ops[start:end], fetch)
		if err != nil {
			return err
		}
		// Only the last chunk covers everything up to the checkpoint.
		move := end == len(ops)
		err = r.send(ctx, c, checkpoint, move)
		if errorNumber(err) == duplicateEntry && !cleared {
			// A unique value passed between two rows that fell into
			// different chunks. Taking every row still to come out of the
			// way settles it for this chunk and all later ones; they are
			// stored again as their chunks are applied.
			rest, _ := r.read(ctx, ops[start:], nil)
			rest.truncates = nil
			if err = r.send(ctx, rest, 0, false); err == nil {
				cleared = true
				err = r.send(ctx, c, checkpoint, move)
			}
		}
		if err != nil {
			return errors.New(message(err))
		}
	}
	return nil
}

// deleteSQL removes n rows of a table by primary key.
func deleteSQL(t *Table, n int) string {
	names := make([]string, len(t.PK))
	binds := make([]string, len(t.PK))
	for i, col := range t.PK {
		names[i], binds[i] = mysql.Quote(t.Cols[col].Target), keyBind(t.Cols[col])
	}
	cols, one := names[0], binds[0]
	if len(names) > 1 {
		cols, one = "("+strings.Join(names, ", ")+")", "("+strings.Join(binds, ", ")+")"
	}
	return "DELETE FROM " + t.QName() + " WHERE " + cols + " IN (" + strings.Repeat(one+", ", n-1) + one + ")"
}

// send applies one chunk in one transaction on the target and, with
// move, stores the checkpoint in it.
func (r *runner) send(ctx context.Context, c *changes, checkpoint int64, move bool) (err error) {
	conn, err := r.applier.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// TRUNCATE commits whatever transaction is open, so it cannot be part
	// of one. It goes first: the operations the stream reported for the
	// table before it are already dropped, and those after it are rows of
	// this chunk or a later one. If the job stops between here and the
	// commit, the changes are read again from the old checkpoint and the
	// table is emptied once more, which does no harm.
	for _, t := range c.truncates {
		if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE "+t.QName()); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	for _, g := range c.groups {
		for start := 0; start < len(g.keys); start += batchRows {
			part := g.keys[start:min(start+batchRows, len(g.keys))]
			args := make([]any, 0, len(part)*len(g.t.PK))
			for _, k := range part {
				args = append(args, k...)
			}
			if _, err := conn.ExecContext(ctx, deleteSQL(g.t, len(part)), args...); err != nil {
				return err
			}
		}
	}
	for _, g := range c.groups {
		load := newLoader(conn, g.t, r.batchLimit)
		for _, row := range g.rows {
			if err := load.add(ctx, row); err != nil {
				return err
			}
		}
		if err := load.flush(ctx); err != nil {
			return err
		}
	}
	if move {
		if _, err := conn.ExecContext(ctx, checkpointUpsert, r.jobID, checkpoint); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (r *runner) anomaly(msg string) {
	r.st.Anomalies++
	if r.st.Anomalies <= 20 {
		r.rep.Event("warn", msg)
	}
}
