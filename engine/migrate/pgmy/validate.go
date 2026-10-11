package pgmy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// keyComparable reports whether a table's primary key renders to the same
// text in both databases: integers, text and UUIDs do.
func keyComparable(t *Table) bool {
	if len(t.PK) == 0 {
		return false
	}
	for _, name := range t.PK {
		if c := t.byName[name]; c == nil || !c.IsKey {
			return false
		}
	}
	return true
}

type tableSum struct {
	count int64
	sum   string
}

// The two statements compute the same thing in each dialect: the row count
// and the sum of a 60-bit slice of the MD5 of each row's primary key. Whole
// rows cannot be compared this way, because the two databases print many
// types differently, but equal counts with equal key sets rule out lost,
// duplicated and stray rows.
func sourceSum(ctx context.Context, src *pgx.Conn, t *Table, keys bool) (tableSum, error) {
	var r tableSum
	name := pgx.Identifier{t.Schema, t.Name}.Sanitize()
	if !keys {
		return r, src.QueryRow(ctx, "SELECT count(*) FROM "+name).Scan(&r.count)
	}
	parts := make([]string, len(t.PK))
	for i, col := range t.PK {
		parts[i] = pgx.Identifier{col}.Sanitize() + "::text"
	}
	err := src.QueryRow(ctx, "SELECT count(*), coalesce(sum(('x' || substr(md5(concat_ws('|', "+strings.Join(parts, ", ")+
		")), 1, 15))::bit(60)::bigint), 0)::text FROM "+name).Scan(&r.count, &r.sum)
	return r, err
}

func targetSum(ctx context.Context, dst *sql.DB, t *Table, keys bool) (tableSum, error) {
	var r tableSum
	if !keys {
		return r, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.QName()).Scan(&r.count)
	}
	err := dst.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(CAST(CONV(SUBSTRING(MD5(CONCAT_WS('|', "+quoteAll(t.PK)+
		")), 1, 15), 16, 10) AS UNSIGNED)), 0) FROM "+t.QName()).Scan(&r.count, &r.sum)
	return r, err
}

// validate compares every table between source and target and returns the
// number that differ.
func validate(ctx context.Context, srcCfg connector.Config, dst *sql.DB, plan *Plan, mode string, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
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
			src, err := postgres.Connect(ctx, srcCfg)
			if err == nil {
				defer src.Close(context.WithoutCancel(ctx))
			}
			for t := range jobs {
				if err != nil {
					fail(err)
					continue
				}
				keys := mode == "checksum" && keyComparable(t)
				var a, b tableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() { defer pair.Done(); b, errB = targetSum(ctx, dst, t, keys) }()
				a, errA = sourceSum(ctx, src, t, keys)
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = fmt.Errorf("%s", message(errB))
					}
					fail(fmt.Errorf("validate %s.%s: %v", t.Schema, t.Name, errA))
					continue
				}
				result := "count and keys match"
				if !keys {
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
