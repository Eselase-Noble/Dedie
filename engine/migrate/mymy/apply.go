package mymy

import (
	"context"
	"errors"
	"fmt"

	driver "github.com/go-sql-driver/mysql"

	"dedie/engine/migrate"
)

// alreadyExists reports errors that mean an interrupted earlier attempt
// already created the object, which makes the DDL phases safe to re-run.
func alreadyExists(err error) bool {
	var myErr *driver.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1050, // table or view exists
		1304, // routine exists
		1359, // trigger exists
		1537: // event exists
		return true
	}
	return false
}

func message(err error) string {
	var myErr *driver.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Message
	}
	return err.Error()
}

// applyStmts runs DDL on the target. Soft statements that fail are retried
// for as long as each pass makes progress, which resolves dependencies
// between views and routines without sorting them. It returns the soft
// statements that still fail.
func applyStmts(ctx context.Context, dst querier, list []Stmt) ([]Stmt, error) {
	var failed []Stmt
	pending := list[:0:0]
	for _, s := range list {
		// Definitions that could not be read from the source go straight
		// to the report.
		if s.SQL == "" {
			failed = append(failed, s)
			continue
		}
		pending = append(pending, s)
	}
	for {
		var retry []Stmt
		progress := false
		for _, s := range pending {
			_, err := dst.ExecContext(ctx, s.SQL)
			if err == nil || alreadyExists(err) {
				progress = true
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !s.Soft {
				return nil, fmt.Errorf("create %s %s: %s", s.Kind, s.Name, message(err))
			}
			s.err = err
			retry = append(retry, s)
		}
		if len(retry) == 0 || !progress {
			return append(failed, retry...), nil
		}
		pending = retry
	}
}

func reportFailed(rep migrate.Reporter, failed []Stmt) {
	for _, s := range failed {
		rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Name, message(s.err)))
	}
}
