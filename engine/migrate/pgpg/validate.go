package pgpg

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// Both sides render values with the same session settings so that equal data
// produces equal text, whatever the server defaults are.
const normalize = `SET TimeZone = 'UTC'; SET DateStyle = 'ISO, MDY'; SET IntervalStyle = 'postgres'; SET bytea_output = 'hex'; SET extra_float_digits = 3`

// checksumSQL returns the row count and an order-independent checksum: the
// sum of a 60-bit slice of each row's MD5. No sort is needed, so it runs as
// one sequential scan per table.
func checksumSQL(t Table) string {
	return `SELECT count(*), coalesce(sum(('x' || substr(md5(t::text), 1, 15))::bit(60)::bigint), 0)::text FROM ` + t.QName() + ` t`
}

type tableSum struct {
	count int64
	sum   string
}

func measure(ctx context.Context, conn *pgx.Conn, t Table, checksum bool) (tableSum, error) {
	var r tableSum
	if checksum {
		return r, conn.QueryRow(ctx, checksumSQL(t)).Scan(&r.count, &r.sum)
	}
	return r, conn.QueryRow(ctx, `SELECT count(*) FROM `+t.QName()).Scan(&r.count)
}

// validate compares every table between source and target and returns the
// number that differ. It is meaningful only once writes have stopped and the
// stream has drained, which is exactly the state cutover puts the job in.
func validate(ctx context.Context, srcCfg, dstCfg connector.Config, plan *Plan, mode string, floatsDiffer bool, rep Reporter) (int, error) {
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
			var src, dst *pgx.Conn
			var err error
			fail := func(e error) { once.Do(func() { firstErr = e }) }
			if src, err = postgres.Connect(ctx, srcCfg); err == nil {
				defer src.Close(context.WithoutCancel(ctx))
				_, err = src.PgConn().Exec(ctx, normalize).ReadAll()
			}
			if err == nil {
				if dst, err = postgres.Connect(ctx, dstCfg); err == nil {
					defer dst.Close(context.WithoutCancel(ctx))
					_, err = dst.PgConn().Exec(ctx, normalize).ReadAll()
				}
			}
			for t := range jobs {
				if err != nil {
					fail(err)
					continue
				}
				// Before PostgreSQL 12 floats print differently, so tables
				// with float columns are compared by row count only.
				checksum := mode == "checksum" && !(floatsDiffer && t.HasFloat)
				var a, b tableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() { defer pair.Done(); a, errA = measure(ctx, src, t, checksum) }()
				b, errB = measure(ctx, dst, t, checksum)
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = errB
					}
					fail(fmt.Errorf("validate %s.%s: %s", t.Schema, t.Name, pgMessage(errA)))
					continue
				}
				result := "match"
				if !checksum {
					result = "count match"
				}
				if a != b {
					result = "mismatch"
					mismatches.Add(1)
				}
				rep.UpdateTable(t.Schema, t.Name, map[string]any{"src_count": a.count, "dst_count": b.count, "validation": result})
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
