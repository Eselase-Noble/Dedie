package mymy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

// checksumSQL returns the row count and an order-independent checksum: the
// sum of a 60-bit slice of each row's MD5. Every column is rendered to text
// first (binary columns as hex) with NULL kept distinct from an empty value.
func checksumSQL(t Table) string {
	parts := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		name := mysql.Quote(c.Name)
		if c.Binary {
			parts[i] = "IFNULL(HEX(" + name + "), '~')"
		} else {
			parts[i] = "IFNULL(CAST(" + name + " AS CHAR CHARACTER SET utf8mb4), '~')"
		}
	}
	return "SELECT COUNT(*), COALESCE(SUM(CAST(CONV(SUBSTRING(MD5(CONCAT_WS('|', " + strings.Join(parts, ", ") +
		")), 1, 15), 16, 10) AS UNSIGNED)), 0) FROM " + t.QName()
}

type tableSum struct {
	count int64
	sum   string
}

func measure(ctx context.Context, db *sql.DB, t Table, checksum bool) (tableSum, error) {
	var r tableSum
	if checksum {
		return r, db.QueryRowContext(ctx, checksumSQL(t)).Scan(&r.count, &r.sum)
	}
	return r, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.QName()).Scan(&r.count)
}

// validate compares every table between source and target and returns the
// number that differ. It is meaningful only once writes have stopped and the
// stream has drained, which is the state cutover puts the job in.
func validate(ctx context.Context, src, dst *sql.DB, plan *Plan, mode string, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
	jobs := make(chan Table)
	var mismatches atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				var a, b tableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() { defer pair.Done(); a, errA = measure(ctx, src, t, mode == "checksum") }()
				b, errB = measure(ctx, dst, t, mode == "checksum")
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = errB
					}
					once.Do(func() { firstErr = fmt.Errorf("validate %s: %s", t.Name, message(errA)) })
					continue
				}
				result := "match"
				if mode != "checksum" {
					result = "count match"
				}
				if a != b {
					result = "mismatch"
					mismatches.Add(1)
				}
				rep.UpdateTable(plan.Database, t.Name, map[string]any{"src_count": a.count, "dst_count": b.count, "validation": result})
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
