package mymy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

var errInterrupted = errors.New("interrupted by operator")

const (
	// A batch is flushed at whichever limit it reaches first.
	batchRows  = 500
	batchBytes = 1 << 20
)

// OpenSnapshot opens one read connection per worker, all looking at the same
// instant, and returns the binary log position of that instant.
//
// A global read lock is held only for as long as it takes to read the
// position and start the transactions, typically a few milliseconds. From
// then on each connection reads its own consistent view while writes on the
// source continue.
func OpenSnapshot(ctx context.Context, src *sql.DB, workers int) ([]*sql.Conn, Position, error) {
	lock, err := src.Conn(ctx)
	if err != nil {
		return nil, Position{}, err
	}
	defer lock.Close()
	if _, err := lock.ExecContext(ctx, "FLUSH TABLES WITH READ LOCK"); err != nil {
		return nil, Position{}, fmt.Errorf("take the global read lock (the source account needs the RELOAD privilege): %s", message(err))
	}
	defer lock.ExecContext(context.WithoutCancel(ctx), "UNLOCK TABLES")

	pos, err := BinlogStatus(ctx, lock)
	if err != nil {
		return nil, pos, err
	}
	conns := make([]*sql.Conn, 0, workers)
	for i := 0; i < workers; i++ {
		c, err := src.Conn(ctx)
		if err == nil {
			_, err = c.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ")
		}
		if err == nil {
			_, err = c.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT")
		}
		if err != nil {
			if c != nil {
				conns = append(conns, c)
			}
			for _, open := range conns {
				open.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
				open.Close()
			}
			return nil, pos, err
		}
		conns = append(conns, c)
	}
	return conns, pos, nil
}

// copyTable streams one table from the source snapshot into the target in
// multi-row INSERT statements.
func copyTable(ctx context.Context, src *sql.Conn, dst *sql.DB, t Table, copied *atomic.Int64) error {
	var cols []Column
	for _, c := range t.Cols {
		if !c.Generated {
			cols = append(cols, c)
		}
	}
	names := strings.Join(t.writable(), ", ")
	rows, err := src.QueryContext(ctx, "SELECT "+names+" FROM "+t.QName())
	if err != nil {
		return err
	}
	defer rows.Close()

	prefix := "INSERT INTO " + t.QName() + " (" + names + ") VALUES "
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
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
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for i, v := range raw {
			switch {
			case v == nil:
				args = append(args, nil)
			case cols[i].Binary:
				args = append(args, append([]byte(nil), v...))
			default:
				// Text arrives in the connection's character set and is
				// converted to the target column's by the target server.
				args = append(args, string(v))
			}
			size += len(v)
		}
		pending++
		if pending >= batchRows || size >= batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// snapshot copies all tables in parallel from the given snapshot connections.
// It stops early when the operator pauses or aborts; the copy then starts
// over on resume, because a snapshot cannot outlive its connections.
func snapshot(ctx context.Context, conns []*sql.Conn, dstCfg connector.Config, plan *Plan, rep migrate.Reporter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Rows arrive in no particular order across tables, so referential and
	// uniqueness checks wait until the data is complete.
	dst, err := mysql.Open(ctx, dstCfg, targetParams(map[string]string{"foreign_key_checks": "0", "unique_checks": "0"}))
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer dst.Close()

	jobs := make(chan Table)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }

	for _, conn := range conns {
		wg.Add(1)
		go func(conn *sql.Conn) {
			defer wg.Done()
			for t := range jobs {
				rep.UpdateTable(plan.Database, t.Name, map[string]any{"status": "copying"})
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
							rep.UpdateTable(plan.Database, t.Name, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyTable(ctx, conn, dst, t, &copied)
				close(done)
				if err != nil {
					rep.UpdateTable(plan.Database, t.Name, map[string]any{"status": "failed", "detail": message(err)})
					fail(fmt.Errorf("copy %s: %s", t.Name, message(err)))
					return
				}
				rep.UpdateTable(plan.Database, t.Name, map[string]any{"status": "copied", "rows_copied": copied.Load()})
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

// truncateTargets empties the target tables before a snapshot is (re)started.
func truncateTargets(ctx context.Context, dstCfg connector.Config, plan *Plan) error {
	dst, err := mysql.Open(ctx, dstCfg, targetParams(map[string]string{"foreign_key_checks": "0"}))
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, t := range plan.Tables {
		if _, err := dst.ExecContext(ctx, "TRUNCATE TABLE "+t.QName()); err != nil {
			return fmt.Errorf("%s: %s", t.Name, message(err))
		}
	}
	return nil
}
