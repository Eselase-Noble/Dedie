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
	// versionsOnly is set once LogMiner is out of the question for this
	// job: every table is then followed through its row versions.
	versionsOnly bool
	// base is where a table's row versions are first read from, and
	// bounds how far each has been read since; see scan.
	base   int64
	bounds map[string]int64
}

// split sorts the tables that can be followed into those read from the
// redo stream and those read from their row versions.
func (f *Follower) split() (mined, versioned []*Table) {
	for _, t := range f.Catalog.Tables {
		switch {
		case len(t.PK) == 0:
			// Reported once, when LogMiner shows a change to it or here.
			if f.versionsOnly && !f.keyless[t.Label()] {
				if f.keyless == nil {
					f.keyless = map[string]bool{}
				}
				f.keyless[t.Label()] = true
				f.Anomaly(fmt.Sprintf("%s has no primary key, so the changes made to it after the copy are not followed", t.Label()))
			}
		case f.versionsOnly || unmined(t):
			versioned = append(versioned, t)
		default:
			mined = append(mined, t)
		}
	}
	return mined, versioned
}

// useVersions gives up on LogMiner for the rest of the job and has every
// table followed through its row versions, from the given SCN on.
func (f *Follower) useVersions(from int64, why string) {
	f.versionsOnly = true
	f.base, f.bounds = from, map[string]int64{}
	f.Reporter.Event("warn", "following changes by reading row versions from the source's undo data instead of its redo logs, because "+why+
		". Nothing is lost by it; each pass reads the primary key index of every table, so the target follows a large or busy source less closely")
}

// minable decides, when the stream starts, whether LogMiner can be used
// at all. It returns the reason when it cannot.
func (f *Follower) minable(ctx context.Context) string {
	if why := MiningBlocked(ctx, f.Source); why != "" {
		return why
	}
	logged, everywhere := KeyLogged(ctx, f.Source, f.Catalog)
	if everywhere {
		return ""
	}
	for _, t := range f.Catalog.Tables {
		if len(t.PK) > 0 && !unmined(t) && !logged[t.Label()] {
			return "primary key supplemental logging is not on for " + t.Label()
		}
	}
	return ""
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

	// What the job has stored a checkpoint for; a table's row versions
	// are read from there when nothing later is known.
	stored := scn
	f.base, f.bounds = scn, map[string]int64{}
	if !f.versionsOnly {
		if why := f.minable(ctx); why != "" {
			f.useVersions(scn, why)
		}
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
	pause := time.Second
	for {
		cur, err := CurrentSCN(ctx, miner)
		if err != nil {
			return "", err
		}
		if cur > scn {
			redo, versioned := f.split()
			m := &mined{}
			if len(redo) > 0 {
				if m, err = f.mine(ctx, miner, scn+1, cur, open); err != nil {
					if !errors.Is(err, errRedoGone) && !errors.Is(err, errNoMiner) {
						return "", err
					}
					// The redo is out of reach, but the undo data still
					// knows what changed since the last checkpoint.
					open = map[string]*openTx{}
					f.useVersions(stored, "LogMiner can no longer read the redo this job needs ("+brief(err.Error())+")")
					continue
				}
			}
			// A restart must read again from the first change of the
			// oldest transaction that has not ended yet.
			checkpoint := cur
			for _, tx := range open {
				if tx.first-1 < checkpoint {
					checkpoint = tx.first - 1
				}
			}
			waiting := false
			if len(versioned) > 0 {
				started := time.Now()
				v, err := f.scan(ctx, reader, versioned, cur)
				if err != nil {
					return "", err
				}
				if len(v.ops) > 0 {
					m.ops = append(m.ops, v.ops...)
					m.commits++
				}
				// Rows found this way are read as of the end of the pass.
				m.lastCommit = cur
				checkpoint, waiting = min(checkpoint, v.low), v.waiting
				// A pass that takes long is not repeated at once.
				pause = min(max(time.Second, 2*time.Since(started)), 30*time.Second)
			}
			sinceCheckpoint++
			if len(m.ops) > 0 || sinceCheckpoint >= 10 {
				if err := f.apply(ctx, reader, m, checkpoint); err != nil {
					return "", err
				}
				stored, sinceCheckpoint = checkpoint, 0
			}
			scn = cur
			if len(m.ops) == 0 && len(open) == 0 && !waiting {
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
		wait := pause
		if draining {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
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
