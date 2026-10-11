package oramy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

var errInterrupted = errors.New("interrupted by operator")

const (
	// A batch of rows is sent at whichever limit it reaches first.
	batchRows  = 500
	batchBytes = 1 << 20
)

// execer is what rows are written through: the pool during the copy, and
// the connection that holds the transaction while changes are applied.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// loader gathers the rows of one table and inserts them several to a
// statement, which is the quickest way into MySQL short of a file the
// server would have to be allowed to read.
type loader struct {
	dst           execer
	prefix, tuple string
	// limit is the size in bytes at which a batch is sent.
	limit      int
	args       []any
	rows, size int
	// loaded is called with the number of rows of each batch stored.
	loaded func(rows int)
}

func newLoader(dst execer, t *Table, limit int) *loader {
	names := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		names[i] = mysql.Quote(c.Target)
	}
	return &loader{dst: dst, limit: limit,
		prefix: "INSERT INTO " + t.QName() + " (" + strings.Join(names, ", ") + ") VALUES ",
		tuple:  "(" + strings.TrimSuffix(strings.Repeat("?,", len(t.Cols)), ",") + ")"}
}

// add takes one row, of values as myValue returns them.
func (l *loader) add(ctx context.Context, row []any) error {
	for _, v := range row {
		switch x := v.(type) {
		case string:
			l.size += len(x)
		case []byte:
			l.size += len(x)
		}
	}
	l.args = append(l.args, row...)
	l.rows++
	if l.rows >= batchRows || l.size >= l.limit {
		return l.flush(ctx)
	}
	return nil
}

// flush sends the rows gathered so far.
func (l *loader) flush(ctx context.Context) error {
	if l.rows == 0 {
		return nil
	}
	stmt := l.prefix + strings.Repeat(l.tuple+",", l.rows-1) + l.tuple
	if _, err := l.dst.ExecContext(ctx, stmt, l.args...); err != nil {
		return err
	}
	if l.loaded != nil {
		l.loaded(l.rows)
	}
	l.args, l.rows, l.size = l.args[:0], 0, 0
	return nil
}

// batchLimit works out how many bytes of values go into one statement.
// A statement must fit into one packet of the size the server accepts,
// and escaping can double the size of a value.
func batchLimit(ctx context.Context, dst *sql.DB) int {
	var packet int64
	if err := dst.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&packet); err != nil || packet <= 0 {
		return batchBytes
	}
	return int(min(batchBytes, max(packet/4, 1<<12)))
}

// copyTable reads one table as of the snapshot SCN and inserts it into
// MySQL in multi-row statements.
func copyTable(ctx context.Context, src, dst *sql.DB, t *Table, scn int64, limit int, copied *atomic.Int64) error {
	rows, err := src.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s AS OF SCN %d", orasrc.SelectList(t.Table), t.Source(), scn))
	if err != nil {
		return snapshotError(err)
	}
	defer rows.Close()

	load := newLoader(dst, t, limit)
	load.loaded = func(n int) { copied.Add(int64(n)) }
	values := make([]any, len(t.Cols))
	dest := make([]any, len(t.Cols))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return snapshotError(err)
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = myValue(t.Cols[i], v)
		}
		if err := load.add(ctx, row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return snapshotError(err)
	}
	return load.flush(ctx)
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
	limit := batchLimit(ctx, dst)
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
				rep.UpdateTable(t.Schema(), t.Target, map[string]any{"status": "copying"})
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
							rep.UpdateTable(t.Schema(), t.Target, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyTable(ctx, src, dst, t, scn, limit, &copied)
				close(done)
				if err != nil {
					if ctx.Err() == nil {
						rep.UpdateTable(t.Schema(), t.Target, map[string]any{"status": "failed", "detail": message(err)})
					}
					fail(fmt.Errorf("copy %s: %s", t.Label(), message(err)))
					return
				}
				rep.UpdateTable(t.Schema(), t.Target, map[string]any{"status": "copied", "rows_copied": copied.Load()})
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

// message reduces a MySQL error to what the server said.
func message(err error) string {
	var myErr *driver.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Message
	}
	return err.Error()
}

// errorNumber is the server's code for a MySQL error, or 0.
func errorNumber(err error) uint16 {
	var myErr *driver.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number
	}
	return 0
}

// alreadyExists reports errors that mean an interrupted earlier attempt
// already created the object, which makes the DDL phases safe to re-run.
func alreadyExists(err error) bool {
	switch errorNumber(err) {
	case 1050, // table exists
		1061, // duplicate index name
		1826, // duplicate foreign key name
		3822: // duplicate check constraint name
		return true
	}
	return false
}
