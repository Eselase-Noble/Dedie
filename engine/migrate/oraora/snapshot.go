package oraora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

var errInterrupted = errors.New("interrupted by operator")

// A batch of the copy is sent when it holds this many rows or this much
// data, whichever comes first, and committed by itself. The second bound
// keeps a table of large objects from being gathered in memory.
const (
	copyRows  = 1000
	copyBytes = 8 << 20
)

// copyTable reads one table as of the snapshot SCN and inserts its rows
// on the target in batches.
//
// The rows go in with ordinary array inserts. A direct-path insert would
// be faster for one table alone, but it locks the table against every
// other writer, commits nothing it can share, and with rows bound from a
// client it leaves each batch in blocks of its own. The tables have no
// indexes or constraints yet, which is what makes the plain insert quick.
func copyTable(ctx context.Context, src, dst *sql.DB, t *Table, scn int64, copied *atomic.Int64) error {
	// The target session reads numbers and dates in the formats the
	// source wrote them in.
	conn, err := orasrc.Pinned(ctx, dst)
	if err != nil {
		return err
	}
	defer conn.Close()

	shape := shapeOf(t)
	rows, err := src.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s AS OF SCN %d", shape.selectList(), t.Source(), scn))
	if err != nil {
		return snapshotError(err)
	}
	defer rows.Close()

	insert := shape.insertSQL()
	batch := newBatch(shape)
	flush := func() error {
		if batch.rows == 0 {
			return nil
		}
		if _, err := conn.ExecContext(ctx, insert, batch.args()...); err != nil {
			return oracle.Friendly(err)
		}
		copied.Add(int64(batch.rows))
		batch.reset()
		return nil
	}

	values := make([]any, shape.params())
	dest := make([]any, len(values))
	for i := range values {
		dest[i] = &values[i]
	}
	empty := make([]bool, len(shape.lobs))
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return snapshotError(err)
		}
		for n := range shape.lobs {
			empty[n] = values[len(t.stored)+n] != nil
		}
		batch.add(values[:len(t.stored)], empty)
		if batch.rows >= copyRows || batch.bytes >= copyBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return snapshotError(err)
	}
	return flush()
}

// snapshotError explains the one failure that is specific to reading the
// past: the undo needed for it was overwritten.
func snapshotError(err error) error {
	if err == nil {
		return nil
	}
	if orasrc.UndoGone(err) {
		return errors.New("the source overwrote the undo data the copy was reading from (ORA-01555, snapshot too old). " +
			"Raise UNDO_RETENTION on the source to longer than the copy takes, or copy with more workers, and run the job again")
	}
	return oracle.Friendly(err)
}

// snapshot copies all tables in parallel, each read as of the same SCN.
// It stops early when the operator pauses or aborts; the copy then
// starts over on resume.
func snapshot(ctx context.Context, src, dst *sql.DB, plan *Plan, scn int64, workers int, rep migrate.Reporter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *Table)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "copying"})
				var copied atomic.Int64
				done := make(chan struct{})
				go func() {
					tick := time.NewTicker(time.Second)
					defer tick.Stop()
					for {
						select {
						case <-done:
							return
						case <-tick.C:
							rep.UpdateTable(t.Schema, t.Name, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyTable(ctx, src, dst, t, scn, &copied)
				close(done)
				if err != nil {
					if ctx.Err() == nil {
						rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "failed", "detail": err.Error()})
					}
					fail(fmt.Errorf("copy %s: %w", t.Label(), err))
					return
				}
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "copied", "rows_copied": copied.Load()})
			}
		}()
	}

	stopWatch, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-tick.C:
				if d := rep.Desired(); d == "pause" || d == "abort" {
					fail(errInterrupted)
					return
				}
			}
		}
	}()

feed:
	for _, t := range plan.Tables {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(stopWatch)
	<-watchDone
	return firstErr
}
