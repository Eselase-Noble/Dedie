package oraora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/orasrc"
)

// applyChunk is how many row changes go into one target transaction, and
// applyRows how many of them are sent in one round trip.
const (
	applyChunk = 5000
	applyRows  = 500
)

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

// ensureCheckpoint creates the checkpoint table if it is not there.
func (r *runner) ensureCheckpoint(ctx context.Context, db oracle.Queryer) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE `+r.plan.checkpointName()+` (
		job_id VARCHAR2(200) PRIMARY KEY, scn NUMBER NOT NULL, updated_at TIMESTAMP DEFAULT SYSTIMESTAMP NOT NULL)`)
	if err != nil && !alreadyThere(err) {
		return fmt.Errorf("prepare target: %w", oracle.Friendly(err))
	}
	return nil
}

// Checkpoint returns the SCN up to which changes have been applied to
// the target, or 0 when none have.
func (r *runner) Checkpoint(ctx context.Context) (int64, error) {
	if err := r.ensureCheckpoint(ctx, r.dst); err != nil {
		return 0, err
	}
	// The checkpoint on the target is the truth: it was committed together
	// with the changes it covers.
	var scn int64
	err := r.dst.QueryRowContext(ctx, `SELECT scn FROM `+r.plan.checkpointName()+` WHERE job_id = :1`, r.jobID).Scan(&scn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, oracle.Friendly(err)
	}
	return scn, nil
}

// change is one thing to do to the target: empty a table, or bring one
// row in line with the source.
type change struct {
	t        *Table
	truncate bool
	// key is the row's primary key as the redo stream carries it.
	key []string
	// row holds the values of the stored columns, or is nil when the
	// source no longer has the row. empty says which of its large
	// objects are empty rather than NULL.
	row   []any
	empty []bool
}

// Apply brings the rows of the committed transactions in line with the
// source and moves the checkpoint, in one target transaction per chunk.
//
// The target has no foreign keys and no triggers until the migration
// ends, so rows can be stored in any order and nothing fires for them.
func (r *runner) Apply(ctx context.Context, ops []orasrc.RowOp, fetch orasrc.Fetch, checkpoint int64) error {
	var changes []change
	for _, op := range ops {
		t := r.plan.of(op.Table)
		if t == nil {
			continue
		}
		if op.Truncate {
			changes = append(changes, change{t: t, truncate: true})
			continue
		}
		row, err := fetch(ctx, op.Table, op.Key)
		if err != nil {
			return err
		}
		c := change{t: t, key: op.Key}
		if row != nil {
			if c.empty, err = r.secondLook(ctx, t, op.Key, row); err != nil {
				return err
			}
			// The row was read with every column; virtual ones are not stored.
			c.row = make([]any, len(t.stored))
			for n, i := range t.stored {
				c.row[n] = row[i]
			}
		}
		// A nil row means the source does not have it: it was deleted, or
		// its key changed. It is made sure to be gone here as well.
		changes = append(changes, c)
	}

	for start := 0; start == 0 || start < len(changes); start += applyChunk {
		end := min(start+applyChunk, len(changes))
		chunk := changes[start:end]
		// Only the last chunk covers everything up to the checkpoint.
		move := end == len(changes)
		err := r.send(ctx, chunk, false, checkpoint, move)
		if hasCode(err, "ORA-00001") {
			// Rows were stored one after another, and a unique value
			// passed from one row to another within the group collided on
			// the way. Removing the group's rows first makes the order
			// irrelevant.
			err = r.send(ctx, chunk, true, checkpoint, move)
		}
		if err != nil {
			return oracle.Friendly(err)
		}
	}
	return nil
}

// secondLook reads again, from the source, what the row as it was
// fetched cannot tell. A large object that came back NULL may be empty
// instead: the driver returns both as NULL. An infinity or a NaN in a
// binary floating-point column comes back as a field of hash signs. For
// the first it returns which large objects are empty; the second it puts
// right in the row.
//
// Only a row with such a value is looked at again, and it is read as it
// is now; should it have changed since the commit being applied, that
// change is still to come and settles it.
func (r *runner) secondLook(ctx context.Context, t *Table, key []string, row []any) ([]bool, error) {
	shape := shapeOf(t)
	empty := make([]bool, len(shape.lobs))
	var exprs []string
	var found []func(sql.NullString)
	for n, i := range shape.lobs {
		if row[i] == nil {
			exprs = append(exprs, emptyFlag(t.Cols[i]))
			found = append(found, func(v sql.NullString) { empty[n] = v.Valid })
		}
	}
	for _, i := range t.stored {
		if binaryFloat(t.Cols[i]) && unprintable(row[i]) {
			exprs = append(exprs, "TO_CHAR("+oracle.Quote(t.Cols[i].Name)+")")
			found = append(found, func(v sql.NullString) {
				row[i] = nil
				if v.Valid {
					row[i] = v.String
				}
			})
		}
	}
	if len(exprs) == 0 {
		return empty, nil
	}
	if r.lookup == nil {
		conn, err := orasrc.Pinned(ctx, r.src)
		if err != nil {
			return nil, err
		}
		r.lookup = conn
	}
	args := make([]any, len(key))
	for i, k := range key {
		args[i] = k
	}
	values := make([]sql.NullString, len(exprs))
	dest := make([]any, len(exprs))
	for i := range values {
		dest[i] = &values[i]
	}
	err := r.lookup.QueryRowContext(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+t.Source()+" WHERE "+orasrc.KeyWhere(t.Table), args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		// Gone since; its deletion is among the changes still to come.
		// Until then the values that could not be read are left out.
		for _, i := range t.stored {
			if binaryFloat(t.Cols[i]) && unprintable(row[i]) {
				row[i] = nil
			}
		}
		return empty, nil
	}
	if err != nil {
		r.lookup.Close()
		r.lookup = nil
		return nil, fmt.Errorf("read %s back from the source: %w", t.Label(), oracle.Friendly(err))
	}
	for i, v := range values {
		found[i](v)
	}
	return empty, nil
}

// send applies changes in one transaction on the target. With clear set,
// every row that is about to be stored is deleted first.
//
// Consecutive changes of one kind to one table are sent together. A
// truncate is DDL in Oracle and commits what the transaction has done so
// far without the checkpoint; that is harmless, because every change
// only makes a row equal to the source's and can be applied again.
func (r *runner) send(ctx context.Context, chunk []change, clear bool, checkpoint int64, move bool) (err error) {
	tx, err := r.applier.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if clear {
		if err = r.deleteRows(ctx, tx, chunk, func(c change) bool { return c.row != nil }); err != nil {
			return err
		}
	}
	for start := 0; start < len(chunk); {
		first := chunk[start]
		end := start + 1
		for end < len(chunk) && end-start < applyRows && chunk[end].t == first.t && !first.truncate && !chunk[end].truncate &&
			(chunk[end].row == nil) == (first.row == nil) {
			end++
		}
		run := chunk[start:end]
		start = end
		switch {
		case first.truncate:
			_, err = tx.ExecContext(ctx, "TRUNCATE TABLE "+first.t.QName())
		case first.row == nil:
			err = r.deleteRows(ctx, tx, run, func(change) bool { return true })
		default:
			shape := shapeOf(first.t)
			batch := newBatch(shape)
			for _, c := range run {
				batch.add(c.row, c.empty)
			}
			_, err = tx.ExecContext(ctx, shape.mergeSQL(), batch.args()...)
		}
		if err != nil {
			return err
		}
	}
	if move {
		if _, err = tx.ExecContext(ctx, `MERGE INTO `+r.plan.checkpointName()+` t USING (SELECT :1 job_id, :2 scn FROM dual) s ON (t.job_id = s.job_id)
			WHEN MATCHED THEN UPDATE SET t.scn = s.scn, t.updated_at = SYSTIMESTAMP
			WHEN NOT MATCHED THEN INSERT (job_id, scn) VALUES (s.job_id, s.scn)`, r.jobID, checkpoint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deleteRows removes the selected rows of a chunk by primary key, the
// rows of one table together.
func (r *runner) deleteRows(ctx context.Context, tx *sql.Tx, chunk []change, selected func(change) bool) error {
	for start := 0; start < len(chunk); {
		t := chunk[start].t
		var keys [][]string
		end := start
		for ; end < len(chunk) && chunk[end].t == t && len(keys) < applyRows; end++ {
			if c := chunk[end]; !c.truncate && selected(c) {
				keys = append(keys, c.key)
			}
		}
		start = end
		if len(keys) == 0 {
			continue
		}
		// The key is bound as text, as the redo stream gave it, one slice
		// per key column.
		args := make([]any, len(t.PK))
		for i := range args {
			if len(keys) == 1 {
				args[i] = keys[0][i]
				continue
			}
			col := make([]string, len(keys))
			for n, key := range keys {
				col[n] = key[i]
			}
			args[i] = col
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t.QName()+" WHERE "+orasrc.KeyWhere(t.Table), args...); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) anomaly(msg string) {
	r.st.Anomalies++
	if r.st.Anomalies <= 20 {
		r.rep.Event("warn", msg)
	}
}
