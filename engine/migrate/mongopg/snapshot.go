package mongopg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// errInterrupted reports that the operator paused or aborted during the copy.
var errInterrupted = errors.New("interrupted")

// docSource feeds a collection's documents to COPY.
type docSource struct {
	ctx     context.Context
	cur     *mongo.Cursor
	copied  *atomic.Int64
	altered *atomic.Int64
	dropped *atomic.Int64
	cols    []column
	values  []any
	err     error
	stop    func() bool
}

func (d *docSource) Next() bool {
	if d.stop() {
		d.err = errInterrupted
		return false
	}
	if !d.cur.Next(d.ctx) {
		d.err = d.cur.Err()
		return false
	}
	values, clean, dropped, err := row(d.cur.Current, d.cols)
	if err != nil {
		d.err = err
		return false
	}
	if !clean {
		d.altered.Add(1)
	}
	d.dropped.Add(int64(dropped))
	d.values = values
	d.copied.Add(1)
	return true
}

func (d *docSource) Values() ([]any, error) { return d.values, nil }
func (d *docSource) Err() error             { return d.err }

// snapshot copies every collection into its table, several at a time. A
// table is emptied first, so a copy that is started again does not
// duplicate anything.
func snapshot(ctx context.Context, db *mongo.Database, dstCfg connector.Config, list []Collection, columns map[string][]column, workers int, rep migrate.Reporter) (altered, dropped int64, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Checking the operator's instruction costs a database read, so it is
	// done once a second for all workers together.
	var interrupted atomic.Bool
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if d := rep.Desired(); d == "pause" || d == "abort" {
					interrupted.Store(true)
				}
			}
		}
	}()

	work := make(chan Collection)
	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex
	var alteredDocs, droppedValues atomic.Int64
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst, err := postgres.Connect(ctx, dstCfg)
			if err != nil {
				fail(fmt.Errorf("target: %w", err))
				return
			}
			defer dst.Close(context.WithoutCancel(ctx))
			for c := range work {
				rep.UpdateTable(targetSchema, c.Name, map[string]any{"status": "copying"})
				var copied atomic.Int64
				done := make(chan struct{})
				go func() {
					tick := time.NewTicker(2 * time.Second)
					defer tick.Stop()
					for {
						select {
						case <-done:
							return
						case <-tick.C:
							rep.UpdateTable(targetSchema, c.Name, map[string]any{"rows_copied": copied.Load()})
						}
					}
				}()
				err := copyCollection(ctx, db, dst, c.Name, columns[c.Name], &copied, &alteredDocs, &droppedValues, interrupted.Load)
				close(done)
				if err != nil {
					if !errors.Is(err, errInterrupted) && ctx.Err() == nil {
						rep.UpdateTable(targetSchema, c.Name, map[string]any{"status": "failed", "detail": err.Error()})
						err = fmt.Errorf("copy %s: %w", c.Name, err)
					}
					fail(err)
					return
				}
				rep.UpdateTable(targetSchema, c.Name, map[string]any{"status": "copied", "rows_copied": copied.Load()})
			}
		}()
	}
feed:
	for _, c := range list {
		select {
		case work <- c:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr == nil && interrupted.Load() {
		firstErr = errInterrupted
	}
	return alteredDocs.Load(), droppedValues.Load(), firstErr
}

func copyCollection(ctx context.Context, db *mongo.Database, dst *pgx.Conn, name string, cols []column, copied, altered, dropped *atomic.Int64, stop func() bool) error {
	if _, err := dst.Exec(ctx, "TRUNCATE "+qname(name)); err != nil {
		return err
	}
	cur, err := db.Collection(name).Find(ctx, bson.D{})
	if err != nil {
		return mongodb.Friendly(err)
	}
	defer cur.Close(context.WithoutCancel(ctx))
	src := &docSource{ctx: ctx, cur: cur, copied: copied, altered: altered, dropped: dropped, cols: cols, stop: stop}
	names := []string{"_id", "doc"}
	for _, c := range cols {
		names = append(names, c.name)
	}
	_, err = dst.CopyFrom(ctx, pgx.Identifier{targetSchema, name}, names, src)
	return err
}
