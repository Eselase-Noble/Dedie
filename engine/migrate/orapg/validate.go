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
)

// plainNumber prints a NUMBER the way PostgreSQL prints a numeric: no
// exponent, and a zero before the decimal point.
const plainNumber = `'FM99999999999999999999999999999999999990.99999999999999999999'`

// keyExprs returns, for each primary key column, an expression in each
// dialect that renders the value as the same text. It reports false when
// some key column has no such pair, in which case only counts can be
// compared.
func keyExprs(t *Table, utf8 bool) (source, target []string, ok bool) {
	if len(t.PK) == 0 {
		return nil, nil, false
	}
	for _, i := range t.PK {
		c := t.Cols[i]
		col := oracle.Quote(c.Name)
		switch {
		case c.Kind == kNumber && (c.DataType == "NUMBER"):
			source = append(source, "RTRIM(TO_CHAR("+col+", "+plainNumber+"), '.')")
		case c.Kind == kText && c.DataType == "VARCHAR2" && utf8:
			source = append(source, col)
		case c.Kind == kText && c.DataType == "CHAR" && utf8:
			// PostgreSQL drops the padding when it prints a char(n).
			source = append(source, "RTRIM("+col+")")
		case c.Kind == kDate:
			source = append(source, "TO_CHAR("+col+", 'YYYY-MM-DD HH24:MI:SS')")
			target = append(target, "to_char("+qident(c.Target)+", 'YYYY-MM-DD HH24:MI:SS')")
			continue
		default:
			return nil, nil, false
		}
		target = append(target, qident(c.Target)+"::text")
	}
	return source, target, true
}

type tableSum struct {
	count int64
	sum   string
}

// The two statements compute the same thing in each dialect: the row
// count and the sum of a 60-bit slice of the MD5 of each row's primary
// key. Whole rows cannot be compared this way, because the two databases
// print many types differently, but equal counts with equal key sets
// rule out lost, duplicated and stray rows.
func sourceSum(ctx context.Context, src *sql.DB, t *Table, keys []string, scn int64) (tableSum, error) {
	var r tableSum
	from := t.Source()
	if scn > 0 {
		from += fmt.Sprintf(" AS OF SCN %d", scn)
	}
	if keys == nil {
		return r, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+from).Scan(&r.count)
	}
	err := src.QueryRowContext(ctx, "SELECT COUNT(*), TO_CHAR(NVL(SUM(TO_NUMBER(SUBSTR(RAWTOHEX(STANDARD_HASH("+
		strings.Join(keys, " || '|' || ")+", 'MD5')), 1, 15), 'XXXXXXXXXXXXXXX')), 0), 'TM9') FROM "+from).Scan(&r.count, &r.sum)
	return r, err
}

func targetSum(ctx context.Context, dst *pgx.Conn, t *Table, keys []string) (tableSum, error) {
	var r tableSum
	if keys == nil {
		return r, dst.QueryRow(ctx, "SELECT count(*) FROM "+t.QName()).Scan(&r.count)
	}
	err := dst.QueryRow(ctx, "SELECT count(*), coalesce(sum(('x' || substr(md5(concat_ws('|', "+strings.Join(keys, ", ")+
		")), 1, 15))::bit(60)::bigint), 0)::text FROM "+t.QName()).Scan(&r.count, &r.sum)
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
	var charset string
	_ = src.QueryRowContext(ctx, `SELECT value FROM nls_database_parameters WHERE parameter = 'NLS_CHARACTERSET'`).Scan(&charset)
	utf8 := charset == "AL32UTF8" || charset == "UTF8"

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
				var a, b tableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() {
					defer pair.Done()
					a, errA = sourceSum(ctx, src, t, srcKeys, scn)
					if scn > 0 && (undoGone(errA) || redefined(errA)) {
						a, errA = sourceSum(ctx, src, t, srcKeys, 0)
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
				rep.UpdateTable(t.Schema, t.Target, map[string]any{"src_count": a.count, "dst_count": b.count, "validation": result})
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
