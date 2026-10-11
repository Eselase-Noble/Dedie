package pgpg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// lineCounter counts rows in a COPY text stream: one line per row, since
// newlines inside values are escaped.
type lineCounter struct {
	w io.Writer
	n *atomic.Int64
}

func (c lineCounter) Write(p []byte) (int, error) {
	c.n.Add(int64(bytes.Count(p, []byte{'\n'})))
	return c.w.Write(p)
}

func columnList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = qident(c)
	}
	return strings.Join(q, ", ")
}

// ExportSnapshot opens a read-only transaction on the source and exports its
// snapshot, so that any number of worker connections can read the same
// instant. This is how an offline job gets a consistent copy without a
// replication slot. The snapshot stays valid only while the returned
// connection keeps the transaction open; the caller closes it after the copy.
func ExportSnapshot(ctx context.Context, srcCfg connector.Config) (*pgx.Conn, string, error) {
	conn, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return nil, "", fmt.Errorf("source: %w", err)
	}
	// The transaction sits idle for the whole copy; a server-side limit on
	// idle transactions must not end it.
	conn.Exec(ctx, "SET idle_in_transaction_session_timeout = 0")
	var name string
	if _, err = conn.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err == nil {
		err = conn.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&name)
	}
	if err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return nil, "", fmt.Errorf("export snapshot: %s", pgMessage(err))
	}
	return conn, name, nil
}

// copyTable streams one table from the source to the target. The source side
// reads inside the exported snapshot, so every table reflects the same
// instant: the replication slot's starting point, or for an offline job the
// moment its snapshot was exported.
func copyTable(ctx context.Context, srcCfg, dstCfg connector.Config, snapshot string, t Table, rows *atomic.Int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return err
	}
	defer src.Close(context.WithoutCancel(ctx))
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return err
	}
	defer dst.Close(context.WithoutCancel(ctx))

	begin := "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SET TRANSACTION SNAPSHOT '" + strings.ReplaceAll(snapshot, "'", "''") + "'"
	if _, err := src.PgConn().Exec(ctx, begin).ReadAll(); err != nil {
		return fmt.Errorf("import snapshot: %w", err)
	}

	cols := columnList(t.Cols)
	pr, pw := io.Pipe()
	go func() {
		_, err := src.PgConn().CopyTo(ctx, lineCounter{pw, rows}, "COPY "+t.QName()+" ("+cols+") TO STDOUT")
		pw.CloseWithError(err)
	}()
	_, err = dst.PgConn().CopyFrom(ctx, pr, "COPY "+t.QName()+" ("+cols+") FROM STDIN")
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	return nil
}

// snapshot copies all tables in parallel. It stops early when the operator
// pauses or aborts; the caller restarts the snapshot from scratch in that
// case, because an exported snapshot cannot outlive its connection.
func snapshot(ctx context.Context, srcCfg, dstCfg connector.Config, snapshotName string, plan *Plan, workers int, rep Reporter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan Table)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) {
		once.Do(func() { firstErr = err; cancel() })
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "copying"})
				var rows atomic.Int64
				done := make(chan struct{})
				go func() {
					tick := time.NewTicker(time.Second)
					defer tick.Stop()
					for {
						select {
						case <-done:
							return
						case <-tick.C:
							rep.UpdateTable(t.Schema, t.Name, map[string]any{"rows_copied": rows.Load()})
						}
					}
				}()
				err := copyTable(ctx, srcCfg, dstCfg, snapshotName, t, &rows)
				close(done)
				if err != nil {
					rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "failed", "detail": pgMessage(err)})
					fail(fmt.Errorf("copy %s.%s: %w", t.Schema, t.Name, err))
					return
				}
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"status": "copied", "rows_copied": rows.Load()})
			}
		}()
	}

	// Watch for pause/abort while the copy runs.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
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

// truncateTargets empties the target tables before a snapshot is (re)started.
func truncateTargets(ctx context.Context, dst *pgx.Conn, plan *Plan) error {
	names := make([]string, len(plan.Tables))
	for i, t := range plan.Tables {
		names[i] = t.QName()
	}
	_, err := dst.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" CASCADE")
	return err
}
