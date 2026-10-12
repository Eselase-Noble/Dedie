package postgres

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

// The test needs a PostgreSQL server it may connect to as a superuser:
//
//	DEDIE_TEST_SOURCE=postgres://postgres:secret@127.0.0.1:5432/postgres

func testConfig(t *testing.T) connector.Config {
	raw := os.Getenv("DEDIE_TEST_SOURCE")
	if raw == "" {
		t.Skip("DEDIE_TEST_SOURCE not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	return connector.Config{Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		Username: u.User.Username(), Password: pw, SSLMode: "disable"}
}

// TestTerminateSessions closes idle sessions by age and one session by
// its pid. What is reported closed must be gone by the time the call
// returns, and a session that has not been idle long enough must be left
// alone and not counted.
func TestTerminateSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := testConfig(t)
	admin, err := driver{}.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	s := admin.(*session)

	const app = "dedie-idle-test"
	open := func() int64 {
		t.Helper()
		conn, err := Connect(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		var pid int64
		if err := conn.QueryRow(ctx, "SELECT set_config('application_name', $1, false), pg_backend_pid()", app).Scan(new(string), &pid); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	alive := func(pid int64) bool {
		t.Helper()
		var n int
		if err := s.conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	old1, old2 := open(), open()
	time.Sleep(3 * time.Second)
	fresh := open()

	filter := connector.SessionFilter{Application: app, State: connector.StateIdle, IdleSeconds: 2}
	n, err := s.TerminateSessions(ctx, filter)
	if err != nil || n != 2 {
		t.Fatalf("closed %d session(s) idle for two seconds, want 2: %v", n, err)
	}
	if alive(old1) || alive(old2) {
		t.Error("a session reported closed is still listed")
	}
	if !alive(fresh) {
		t.Error("a session idle for less than the given time was closed")
	}
	if n, err := s.TerminateSessions(ctx, filter); err != nil || n != 0 {
		t.Errorf("a second pass closed %d session(s): %v", n, err)
	}

	if err := s.CancelBackend(ctx, fresh, true); err != nil {
		t.Fatal(err)
	}
	if alive(fresh) {
		t.Error("the terminated session is still listed")
	}
	if err := s.CancelBackend(ctx, fresh, true); err == nil {
		t.Error("terminating a session that is gone reported success")
	}
}

// TestResetSession leaves a session changed in the ways a typed statement
// can change one, and expects Reset to put all of it back.
func TestResetSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := testConfig(t)
	started := time.Now()
	opened, err := driver{}.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	connect := time.Since(started)
	defer opened.Close(ctx)
	s := opened.(*session)
	show := func(setting string) string {
		t.Helper()
		var v string
		if err := s.conn.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	path, timeout := show("search_path"), show("statement_timeout")

	for _, stmt := range []string{"SET search_path = pg_catalog", "SET statement_timeout = '7s'", "CREATE TEMP TABLE dedie_reset_t (a int)", "BEGIN", "SELECT 1/0"} {
		// The last one fails, leaving the transaction aborted.
		s.conn.Exec(ctx, stmt)
	}
	started = time.Now()
	if !s.Reset(ctx) || !s.Alive(ctx) {
		t.Fatal("the session could not be reset")
	}
	t.Logf("opening a session took %s; resetting and checking one took %s", connect.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
	if got := show("search_path"); got != path {
		t.Errorf("search_path is %q after the reset, was %q", got, path)
	}
	if got := show("statement_timeout"); got != timeout {
		t.Errorf("statement_timeout is %q after the reset, was %q", got, timeout)
	}
	var n int
	if err := s.conn.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname = 'dedie_reset_t'").Scan(&n); err != nil || n != 0 {
		t.Errorf("the temporary table outlived the reset: %d, %v", n, err)
	}
	if s.conn.PgConn().TxStatus() != 'I' {
		t.Error("a transaction is still open after the reset")
	}
}
