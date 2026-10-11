package orapg

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

var errInterrupted = errors.New("interrupted by operator")

// copyTable streams one table, read as of the snapshot SCN, into
// PostgreSQL's COPY.
func copyTable(ctx context.Context, src *sql.DB, dstCfg connector.Config, t *Table, scn int64, copied *atomic.Int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return err
	}
	defer dst.Close(context.WithoutCancel(ctx))

	targets := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		targets[i] = qident(c.Target)
	}
	rows, err := src.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s AS OF SCN %d", selectList(t), t.Source(), scn))
	if err != nil {
		return snapshotError(err)
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		out := bufio.NewWriterSize(pw, 1<<16)
		values := make([]any, len(t.Cols))
		dest := make([]any, len(t.Cols))
		for i := range values {
			dest[i] = &values[i]
		}
		var err error
		for err == nil && rows.Next() {
			if err = rows.Scan(dest...); err != nil {
				break
			}
			for i, v := range values {
				if i > 0 {
					out.WriteByte('\t')
				}
				out.WriteString(copyField(t.Cols[i], v))
			}
			err = out.WriteByte('\n')
			copied.Add(1)
		}
		if err == nil {
			err = rows.Err()
		}
		if err == nil {
			err = out.Flush()
		}
		pw.CloseWithError(snapshotError(err))
	}()

	_, err = dst.PgConn().CopyFrom(ctx, pr, "COPY "+t.QName()+" ("+strings.Join(targets, ", ")+") FROM STDIN")
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	return nil
}

// snapshotError explains the one failure that is specific to reading the
// past: the undo needed for it was overwritten.
func snapshotError(err error) error {
	if err == nil {
		return nil
	}
	if undoGone(err) {
		return errors.New("the source overwrote the undo data the copy was reading from (ORA-01555, snapshot too old). " +
			"Raise UNDO_RETENTION on the source to longer than the copy takes, or copy with more workers, and run the job again")
	}
	return oracle.Friendly(err)
}

// snapshot copies all tables in parallel, each read as of the same SCN.
// It stops early when the operator pauses or aborts; the copy then
// starts over on resume.
func snapshot(ctx context.Context, src *sql.DB, dstCfg connector.Config, plan *Plan, scn int64, workers int, rep migrate.Reporter) error {
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
				rep.UpdateTable(t.Schema, t.Target, map[string]any{"status": "copying"})
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
							rep.UpdateTable(t.Schema, t.Target, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyTable(ctx, src, dstCfg, t, scn, &copied)
				close(done)
				if err != nil {
					if ctx.Err() == nil {
						rep.UpdateTable(t.Schema, t.Target, map[string]any{"status": "failed", "detail": pgMessage(err)})
					}
					fail(fmt.Errorf("copy %s: %s", t.Label(), pgMessage(err)))
					return
				}
				rep.UpdateTable(t.Schema, t.Target, map[string]any{"status": "copied", "rows_copied": copied.Load()})
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

func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Detail != "" {
			return pgErr.Message + " (" + pgErr.Detail + ")"
		}
		return pgErr.Message
	}
	return err.Error()
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func alreadyExists(err error) bool {
	switch pgCode(err) {
	case "42P07", "42710", "42P06", "42P16":
		return true
	}
	return false
}
