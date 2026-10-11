package orasrc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"dedie/engine/migrate"
)

// Reasons Follow stops without an error.
const (
	StopPause   = "pause"
	StopAbort   = "abort"
	StopCutover = "cutover"
)

// Fetch reads one row of the source by primary key, in the form the copy
// uses: one value per column of the table, as SelectList reads them. It
// returns nil when the source does not have the row.
type Fetch func(ctx context.Context, t *Table, key []string) ([]any, error)

// Target is the side of a pipeline that stores the changes Follow finds.
type Target interface {
	// Checkpoint returns the SCN stored on the target by the last Apply,
	// or 0 when nothing has been applied yet. The checkpoint on the
	// target is the truth: it was committed together with the changes it
	// covers.
	Checkpoint(ctx context.Context) (int64, error)
	// Apply brings the given rows in line with the source and stores the
	// checkpoint. For each operation it either empties the table, or
	// fetches the row and stores it whole, or, when fetch returns nil,
	// makes sure the row is gone: it was deleted, or its key changed.
	// The operations hold at most one per row. The checkpoint must
	// become durable in the same target transaction as the last of the
	// rows and not before; ops may be empty, when only the checkpoint
	// moves.
	Apply(ctx context.Context, ops []RowOp, fetch Fetch, checkpoint int64) error
}

// Follower follows the changes made to the source after the copy and
// hands them to a Target. One is made per job run and kept across
// restarts of the stream, so that what is reported once is reported once.
// All of its exported fields must be set.
type Follower struct {
	Source   *sql.DB
	Catalog  *Catalog
	Target   Target
	Reporter migrate.Reporter

	// Advanced is called after each pass over the redo stream with the
	// SCN the target is now consistent up to and the age in seconds of
	// the last change applied, which is zero once the source is quiet.
	Advanced func(scn, lagSeconds int64)
	// Applied is called once changes are committed on the target, with
	// the number of source transactions and of rows they came to.
	Applied func(transactions, rows int64)
	// Anomaly is called for each change that could not be followed.
	Anomaly func(message string)
	// Save is called about once a second, for the pipeline to store its
	// state.
	Save func()

	// keyless remembers the tables already reported as not followed.
	keyless    map[string]bool
	undoWarned bool
}

// Follow reads the source's redo with LogMiner and applies what it finds
// until the operator pauses, aborts or cuts over. It returns why it
// stopped. start is where the redo stream is first read from when the
// target holds no checkpoint yet: the mineFrom of StartPosition.
func (f *Follower) Follow(ctx context.Context, start int64) (string, error) {
	scn, err := f.Target.Checkpoint(ctx)
	if err != nil {
		return "", err
	}
	if scn == 0 {
		// Nothing applied yet: start where the copy says to.
		scn = start - 1
	}

	// LogMiner belongs to a session, so mining keeps one connection for
	// itself and rows are read back on another.
	miner, err := Pinned(ctx, f.Source)
	if err != nil {
		return "", err
	}
	defer miner.Close()
	reader, err := Pinned(ctx, f.Source)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	open := map[string]*openTx{}
	draining, quiet, sinceCheckpoint := false, 0, 0
	for {
		cur, err := CurrentSCN(ctx, miner)
		if err != nil {
			return "", err
		}
		if cur > scn {
			m, err := f.mine(ctx, miner, scn+1, cur, open)
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
				if err := f.apply(ctx, reader, m, checkpoint); err != nil {
					return "", err
				}
				sinceCheckpoint = 0
			}
			scn = cur
			if len(m.ops) == 0 && len(open) == 0 {
				quiet++
				f.Advanced(scn, 0)
			} else {
				quiet = 0
				f.Advanced(scn, m.lagSeconds)
			}
		}
		f.Save()

		switch f.Reporter.Desired() {
		case "pause":
			return StopPause, nil
		case "abort":
			return StopAbort, nil
		case "cutover":
			if !draining {
				draining, quiet = true, 0
				f.Reporter.Event("info", "cutover requested: applying the remaining changes")
			}
		}
		// Two passes in a row that found nothing to apply and no
		// transaction in flight mean the source has gone quiet.
		if draining && quiet >= 2 {
			return StopCutover, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// apply hands the rows of the committed transactions to the target,
// together with the checkpoint that covers them.
func (f *Follower) apply(ctx context.Context, reader *sql.Conn, m *mined, checkpoint int64) error {
	ops := Coalesce(m.ops)
	// Read the row as it stood when the last of these transactions
	// committed, so the target passes through states the source was
	// really in. If that moment cannot be read any more, the row as it is
	// now is the next best thing: later changes are still to come.
	fetch := func(ctx context.Context, t *Table, key []string) ([]any, error) {
		row, err := FetchRow(ctx, reader, t, key, m.lastCommit)
		if errors.Is(err, ErrUndoGone) || errors.Is(err, ErrRedefined) {
			if errors.Is(err, ErrUndoGone) && !f.undoWarned {
				f.undoWarned = true
				f.Reporter.Event("warn", "the source no longer keeps undo data back to the changes being applied; rows are read as they are now instead. Raise UNDO_RETENTION if this job often runs behind")
			}
			row, err = FetchRow(ctx, reader, t, key, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s back from the source: %w", t.Label(), err)
		}
		return row, nil
	}
	if err := f.Target.Apply(ctx, ops, fetch, checkpoint); err != nil {
		return err
	}
	if len(ops) > 0 {
		f.Applied(m.commits, int64(len(ops)))
	}
	return nil
}
