package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// validate compares every table between source and target and returns
// the number that differ. Both sides are asked the same question in the
// same words: the row count and, where the primary key allows it, the
// sum over the keys that orasrc.SourceSum computes.
//
// With an SCN the source is read as of that moment, which is how an
// offline copy is checked against exactly what it copied; if the source
// no longer holds that moment, it is read as it is now.
func validate(ctx context.Context, src, dst *sql.DB, plan *Plan, mode string, scn int64, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
	// The sum is over the bytes of the keys, so text keys can only be
	// compared when both databases store text the same way.
	srcText, _ := charset(ctx, src)
	dstText, _ := charset(ctx, dst)
	sameText := srcText != "" && srcText == dstText

	jobs := make(chan *Table)
	var mismatches atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	fail := func(err error) { once.Do(func() { firstErr = err }) }

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				var keys []string
				if mode == "checksum" {
					keys, _ = orasrc.KeyExprs(t.Table, sameText)
				}
				// The same table under the name it has on the target.
				there := *t.Table
				there.Owner = t.Schema

				var a, b orasrc.TableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() {
					defer pair.Done()
					a, errA = orasrc.SourceSum(ctx, src, t.Table, keys, scn)
					if scn > 0 && (orasrc.UndoGone(errA) || orasrc.Redefined(errA)) {
						a, errA = orasrc.SourceSum(ctx, src, t.Table, keys, 0)
					}
				}()
				b, errB = orasrc.SourceSum(ctx, dst, &there, keys, 0)
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = errB
					}
					fail(fmt.Errorf("validate %s: %w", t.Label(), oracle.Friendly(errA)))
					continue
				}
				result := "count and keys match"
				if keys == nil {
					result = "count match"
				}
				if a != b {
					result = "mismatch"
					mismatches.Add(1)
				}
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"src_count": a.Count, "dst_count": b.Count, "validation": result})
			}
		}()
	}
	for _, t := range plan.Tables {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	return int(mismatches.Load()), firstErr
}
