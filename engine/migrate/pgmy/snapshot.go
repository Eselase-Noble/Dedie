package pgmy

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

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

var errInterrupted = errors.New("interrupted by operator")

const (
	// A batch is flushed at whichever limit it reaches first.
	batchRows  = 500
	batchBytes = 1 << 20
)

// sessionSetup makes the source render values the same way the replication
// stream does, so the snapshot and the stream need one conversion.
const sessionSetup = `SET TimeZone = 'UTC'; SET DateStyle = 'ISO, MDY'; SET IntervalStyle = 'postgres'; SET bytea_output = 'hex'; SET extra_float_digits = 3`

// copyTable streams one table out of the source snapshot with COPY and
// inserts it into MySQL in multi-row statements.
func copyTable(ctx context.Context, srcCfg connector.Config, dst *sql.DB, snapshot string, t *Table, copied *atomic.Int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return err
	}
	defer src.Close(context.WithoutCancel(ctx))
	begin := "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SET TRANSACTION SNAPSHOT '" + strings.ReplaceAll(snapshot, "'", "''") + "'; " + sessionSetup
	if _, err := src.PgConn().Exec(ctx, begin).ReadAll(); err != nil {
		return fmt.Errorf("import snapshot: %w", err)
	}

	source := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		source[i] = pgx.Identifier{c.Name}.Sanitize()
	}
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := src.PgConn().CopyTo(ctx, pw, "COPY "+pgx.Identifier{t.Schema, t.Name}.Sanitize()+" ("+strings.Join(source, ", ")+") TO STDOUT")
		pw.CloseWithError(err)
	}()

	prefix := "INSERT INTO " + t.QName() + " (" + quoteAll(names) + ") VALUES "
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(t.Columns)), ",") + ")"
	var args []any
	pending, size := 0, 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		stmt := prefix + strings.TrimSuffix(strings.Repeat(tuple+",", pending), ",")
		if _, err := dst.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
		copied.Add(int64(pending))
		args, pending, size = args[:0], 0, 0
		return nil
	}

	reader := bufio.NewReaderSize(pr, 1<<16)
	var fields [][]byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			fields = copyFields(line[:len(line)-1], fields)
			if len(fields) != len(t.Columns) {
				pr.CloseWithError(errInterrupted)
				return fmt.Errorf("expected %d columns from the source, got %d", len(t.Columns), len(fields))
			}
			for i, f := range fields {
				if f == nil {
					args = append(args, nil)
				} else {
					args = append(args, convert(&t.Columns[i], f))
				}
				size += len(f)
			}
			pending++
			if pending >= batchRows || size >= batchBytes {
				if ferr := flush(); ferr != nil {
					pr.CloseWithError(ferr)
					return ferr
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return flush()
}

// snapshot copies all tables in parallel. It stops early when the operator
// pauses or aborts; the copy then starts over on resume, because an exported
// snapshot cannot outlive its connection.
func snapshot(ctx context.Context, srcCfg connector.Config, dst *sql.DB, snapshotName string, plan *Plan, workers int, rep migrate.Reporter) error {
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
				err := copyTable(ctx, srcCfg, dst, snapshotName, t, &copied)
				close(done)
				if err != nil {
					rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "failed", "detail": message(err)})
					fail(fmt.Errorf("copy %s.%s: %s", t.Schema, t.Name, message(err)))
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
