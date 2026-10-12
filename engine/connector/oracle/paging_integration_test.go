package oracle

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dedie/engine/connector"
)

// TestTablePaging runs the statements the table browser sends for the
// first, a middle and the last page of a table, read-only as it does,
// and expects each row exactly once across the pages.
//
//	DEDIE_TEST_ORACLE_SOURCE=oracle://dedie_test:secret@127.0.0.1:1521/FREEPDB1
func TestTablePaging(t *testing.T) {
	raw := os.Getenv("DEDIE_TEST_ORACLE_SOURCE")
	if raw == "" {
		t.Skip("DEDIE_TEST_ORACLE_SOURCE not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	cfg := connector.Config{Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		Username: u.User.Username(), Password: pw, SSLMode: "disable"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.ExecContext(ctx, `DROP TABLE dedie_pg_rows PURGE`)
	if _, err := db.ExecContext(ctx, `CREATE TABLE dedie_pg_rows AS SELECT LEVEL id, 'row ' || LEVEL note FROM dual CONNECT BY LEVEL <= 250`); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DROP TABLE dedie_pg_rows PURGE`)

	drv, err := connector.Get(Kind)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(ctx)
	const pageSize = 100
	seen := map[string]bool{}
	for page, want := range []int{100, 100, 50, 0} {
		sql := fmt.Sprintf(`SELECT * FROM %s ORDER BY "ID" OFFSET %d ROWS FETCH NEXT %d ROWS ONLY`, QName(strings.ToUpper(cfg.Username), "DEDIE_PG_ROWS"), page*pageSize, pageSize)
		res, err := sess.Query(ctx, sql, connector.QueryOptions{ReadOnly: true, MaxRows: pageSize})
		if err != nil {
			t.Fatalf("page %d: %v", page+1, err)
		}
		if len(res) != 1 || len(res[0].Rows) != want {
			t.Fatalf("page %d: %d row(s), want %d", page+1, len(res[0].Rows), want)
		}
		for _, row := range res[0].Rows {
			id := *row[0]
			if seen[id] {
				t.Errorf("row %s is on two pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 250 {
		t.Errorf("the pages hold %d distinct row(s), want 250", len(seen))
	}
}
