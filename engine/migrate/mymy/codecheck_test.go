package mymy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	driver "github.com/go-sql-driver/mysql"
)

// refusing is a target that answers every statement with one error.
type refusing struct{ err error }

func (r refusing) ExecContext(context.Context, string, ...any) (sql.Result, error) { return nil, r.err }
func (r refusing) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, r.err
}
func (r refusing) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func TestCodeExecTellsRefusalsApart(t *testing.T) {
	run := func(err error) (bool, error) {
		return CodeExec(refusing{err})(context.Background(), "CREATE FUNCTION f() RETURNS INT RETURN 1")
	}
	// The binary log's objection is put in words an administrator can act on.
	_, err := run(&driver.MySQLError{Number: 1418, Message: "This function has none of DETERMINISTIC, NO SQL, or READS SQL DATA in its declaration and binary logging is enabled (you *might* want to use the less safe log_bin_trust_function_creators variable)"})
	if err == nil || err.Error() != BinlogRefusal || !strings.Contains(err.Error(), "log_bin_trust_function_creators is off") {
		t.Errorf("error 1418 is reported as: %v", err)
	}
	// A routine or trigger that is already there is not a refusal.
	for _, number := range []uint16{1304, 1359} {
		if existed, err := run(&driver.MySQLError{Number: number, Message: "already exists"}); !existed || err != nil {
			t.Errorf("error %d: existed %v, %v", number, existed, err)
		}
	}
	if existed, err := run(&driver.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}); existed || err == nil || !strings.Contains(err.Error(), "SQL syntax") {
		t.Errorf("a syntax error: existed %v, %v", existed, err)
	}
	if existed, err := run(errors.New("connection lost")); existed || err == nil {
		t.Errorf("a lost connection: existed %v, %v", existed, err)
	}
}
