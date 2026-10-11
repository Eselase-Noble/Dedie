package mypg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
)

// keyComparable reports whether a table's primary key renders to the same
// text in both databases, which is what a cross-engine checksum needs. That
// holds for integers and for text stored as UTF-8.
func keyComparable(t mymy.Table) bool {
	if len(t.PK) == 0 {
		return false
	}
	for _, i := range t.PK {
		c := t.Cols[i]
		switch c.DataType {
		case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		case "char", "varchar":
			if !strings.HasPrefix(c.Charset, "utf8") && c.Charset != "ascii" {
				return false
			}
		default:
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
func sourceSum(ctx context.Context, src *sql.DB, t mymy.Table, keys bool) (tableSum, error) {
	var r tableSum
	if !keys {
		return r, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.QName()).Scan(&r.count)
	}
	parts := make([]string, len(t.PK))
	for i, col := range t.PK {
		parts[i] = mysql.Quote(t.Cols[col].Name)
	}
	err := src.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(CAST(CONV(SUBSTRING(MD5(CONCAT_WS('|', "+strings.Join(parts, ", ")+
		")), 1, 15), 16, 10) AS UNSIGNED)), 0) FROM "+t.QName()).Scan(&r.count, &r.sum)
	return r, err
}

func targetSum(ctx context.Context, dst *pgx.Conn, t mymy.Table, keys bool) (tableSum, error) {
	var r tableSum
	if !keys {
		return r, dst.QueryRow(ctx, "SELECT count(*) FROM "+qname(t.Name)).Scan(&r.count)
	}
	parts := make([]string, len(t.PK))
	for i, col := range t.PK {
		parts[i] = qident(t.Cols[col].Name) + "::text"
	}
	err := dst.QueryRow(ctx, "SELECT count(*), coalesce(sum(('x' || substr(md5(concat_ws('|', "+strings.Join(parts, ", ")+
		")), 1, 15))::bit(60)::bigint), 0)::text FROM "+qname(t.Name)).Scan(&r.count, &r.sum)
	return r, err
}

// validate compares every table between source and target and returns the
// number that differ.
func validate(ctx context.Context, src *sql.DB, dstCfg connector.Config, plan *Plan, mode string, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
	jobs := make(chan mymy.Table)
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
				keys := mode == "checksum" && keyComparable(t)
				var a, b tableSum
				var errA, errB error
				var pair sync.WaitGroup
				pair.Add(1)
				go func() { defer pair.Done(); a, errA = sourceSum(ctx, src, t, keys) }()
				b, errB = targetSum(ctx, dst, t, keys)
				pair.Wait()
				if errA != nil || errB != nil {
					if errA == nil {
						errA = errB
					}
					fail(fmt.Errorf("validate %s: %s", t.Name, pgMessage(errA)))
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
				rep.UpdateTable(targetSchema, t.Name, map[string]any{"src_count": a.count, "dst_count": b.count, "validation": result})
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
