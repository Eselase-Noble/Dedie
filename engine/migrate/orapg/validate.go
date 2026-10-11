package orapg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// keyExprs returns, for each primary key column, an expression in each
// dialect that renders the value as the same text. It reports false when
// some key column has no such pair, in which case only counts can be
// compared.
func keyExprs(t *Table, utf8 bool) (source, target []string, ok bool) {
	source, ok = orasrc.KeyExprs(t.Table, utf8)
	if !ok {
		return nil, nil, false
	}
	for _, i := range t.PK {
		c := t.Cols[i]
		if c.Kind == orasrc.KindDate {
			target = append(target, "to_char("+qident(c.Target)+", 'YYYY-MM-DD HH24:MI:SS')")
			continue
		}
		// A numeric prints as the source's plain number does, and
		// PostgreSQL drops the padding when it prints a char(n).
		target = append(target, qident(c.Target)+"::text")
	}
	return source, target, true
}

// targetSum computes on PostgreSQL what orasrc.SourceSum computes on the
// source: the row count and the sum of a 60-bit slice of the MD5 of each
// row's primary key.
func targetSum(ctx context.Context, dst *pgx.Conn, t *Table, keys []string) (orasrc.TableSum, error) {
	var r orasrc.TableSum
	if keys == nil {
		return r, dst.QueryRow(ctx, "SELECT count(*) FROM "+t.QName()).Scan(&r.Count)
	}
	err := dst.QueryRow(ctx, "SELECT count(*), coalesce(sum(('x' || substr(md5(concat_ws('|', "+strings.Join(keys, ", ")+
		")), 1, 15))::bit(60)::bigint), 0)::text FROM "+t.QName()).Scan(&r.Count, &r.Sum)
	return r, err
}

// validate compares every table between source and target and returns
// the number that differ. With an SCN the source is read as of that
// moment, which is how an offline copy is checked against exactly what
// it copied; if the source no longer holds that moment, it is read as it
// is now.
func validate(ctx context.Context, src *sql.DB, dstCfg connector.Config, plan *Plan, mode string, scn int64, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
	utf8 := orasrc.Unicode(ctx, src)

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
			dst, err := postgres.Connect(ctx, dstCfg)
			if err == nil {
				defer dst.Close(context.WithoutCancel(ctx))
			}
			for t := range jobs {
				if err != nil {
					fail(err)
					continue
				}
				var srcKeys, dstKeys []string
				if mode == "checksum" {
					srcKeys, dstKeys, _ = keyExprs(t, utf8)
				}
				var a, b orasrc.TableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() {
					defer pair.Done()
					a, errA = orasrc.SourceSum(ctx, src, t.Table, srcKeys, scn)
					if scn > 0 && (orasrc.UndoGone(errA) || orasrc.Redefined(errA)) {
						a, errA = orasrc.SourceSum(ctx, src, t.Table, srcKeys, 0)
					}
				}()
				b, errB = targetSum(ctx, dst, t, dstKeys)
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = errors.New(pgMessage(errB))
					}
					fail(fmt.Errorf("validate %s: %s", t.Label(), oracle.Friendly(errA)))
					continue
				}
				result := "count and keys match"
				if srcKeys == nil {
					result = "count match"
				}
				if a != b {
					result = "mismatch"
					mismatches.Add(1)
				}
				rep.UpdateTable(t.Schema, t.Target, map[string]any{"src_count": a.Count, "dst_count": b.Count, "validation": result})
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
