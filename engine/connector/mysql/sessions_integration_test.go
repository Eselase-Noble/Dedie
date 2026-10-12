package mysql

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dedie/engine/connector"
)

// TestSessionAfterReadOnlyQuery uses one session the way a kept one is
// used: a read-only statement, then work that writes. The read-only
// statement must leave nothing behind that stops the write.
//
//	DEDIE_TEST_MYSQL_SOURCE=mysql://root:secret@127.0.0.1:3306/dedie_test
func TestSessionAfterReadOnlyQuery(t *testing.T) {
	raw := os.Getenv("DEDIE_TEST_MYSQL_SOURCE")
	if raw == "" {
		t.Skip("DEDIE_TEST_MYSQL_SOURCE not set")
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
	drv, err := connector.Get(Kind)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(ctx)
	run := func(stmt string, readOnly bool) error {
		_, err := sess.Query(ctx, stmt, connector.QueryOptions{ReadOnly: readOnly})
		return err
	}
	// A database of the test's own, so that the connection need not name one.
	if err := run("CREATE DATABASE IF NOT EXISTS dedie_ro_check", false); err != nil {
		t.Fatal(err)
	}
	defer run("DROP DATABASE IF EXISTS dedie_ro_check", false)
	if err := run("CREATE TABLE dedie_ro_check.dedie_ro_check (id INT PRIMARY KEY)", false); err != nil {
		t.Fatal(err)
	}
	if err := run("SELECT COUNT(*) FROM dedie_ro_check.dedie_ro_check", true); err != nil {
		t.Fatal(err)
	}
	if err := run("INSERT INTO dedie_ro_check.dedie_ro_check VALUES (1)", true); err == nil {
		t.Error("a write was accepted in read-only mode")
	}
	if err := run("INSERT INTO dedie_ro_check.dedie_ro_check VALUES (2)", false); err != nil {
		t.Errorf("a write after a read-only statement on the same session: %v", err)
	}
	res, err := sess.Query(ctx, "SELECT @@session.transaction_read_only", connector.QueryOptions{})
	if err != nil || len(res) != 1 || len(res[0].Rows) != 1 || *res[0].Rows[0][0] != "0" {
		t.Errorf("the session is still read-only: %v", err)
	}
	if a, ok := sess.(connector.Reusable); !ok || !a.Alive(ctx) {
		t.Error("the session does not answer after the statements")
	}
}
