package mypg

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

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

var errInterrupted = errors.New("interrupted by operator")

// copyTable streams one table from the source snapshot into PostgreSQL's
// COPY, converting each value on the way.
func copyTable(ctx context.Context, src *sql.Conn, dstCfg connector.Config, t mymy.Table, copied *atomic.Int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return err
	}
	defer dst.Close(context.WithoutCancel(ctx))

	selects := make([]string, len(t.Cols))
	targets := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		selects[i] = mysql.Quote(c.Name)
		if c.DataType == "bit" {
			// Read bit fields as the number they represent.
			selects[i] = "CAST(" + selects[i] + " AS UNSIGNED)"
		}
		targets[i] = qident(c.Name)
	}
	rows, err := src.QueryContext(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM "+t.QName())
	if err != nil {
		return err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		out := bufio.NewWriterSize(pw, 1<<16)
		raw := make([]sql.RawBytes, len(t.Cols))
		dest := make([]any, len(t.Cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		var err error
		for err == nil && rows.Next() {
			if err = rows.Scan(dest...); err != nil {
				break
			}
			for i, v := range raw {
				if i > 0 {
					out.WriteByte('\t')
				}
				out.WriteString(copyValue(t.Cols[i], v))
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
		pw.CloseWithError(err)
	}()

	_, err = dst.PgConn().CopyFrom(ctx, pr, "COPY "+qname(t.Name)+" ("+strings.Join(targets, ", ")+") FROM STDIN")
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	return nil
}

// snapshot copies all tables in parallel from the given snapshot connections.
// It stops early when the operator pauses or aborts; the copy then starts
// over on resume, because a snapshot cannot outlive its connections.
func snapshot(ctx context.Context, conns []*sql.Conn, dstCfg connector.Config, plan *Plan, rep migrate.Reporter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan mymy.Table)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }

	for _, conn := range conns {
		wg.Add(1)
		go func(conn *sql.Conn) {
			defer wg.Done()
			for t := range jobs {
				rep.UpdateTable(targetSchema, t.Name, map[string]any{"status": "copying"})
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
							rep.UpdateTable(targetSchema, t.Name, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyTable(ctx, conn, dstCfg, t, &copied)
				close(done)
				if err != nil {
					rep.UpdateTable(targetSchema, t.Name, map[string]any{"status": "failed", "detail": pgMessage(err)})
					fail(fmt.Errorf("copy %s: %s", t.Name, pgMessage(err)))
					return
				}
				rep.UpdateTable(targetSchema, t.Name, map[string]any{"status": "copied", "rows_copied": copied.Load()})
			}
		}(conn)
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
