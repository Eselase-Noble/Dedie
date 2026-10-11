package pgpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// alreadyExists reports errors that mean an earlier, interrupted attempt
// already created the object. Treating them as success makes the DDL phases
// safe to re-run.
func alreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P07", "42710", "42P06", "42723", "42P16":
		return true
	}
	return false
}

func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// applyStmts runs DDL on the target. Soft statements that fail are retried
// for as long as each pass makes progress, which resolves dependencies
// between views, functions and types without sorting them. It returns the
// soft statements that still fail.
func applyStmts(ctx context.Context, dst *pgx.Conn, list []Stmt) ([]Stmt, error) {
	if _, err := dst.Exec(ctx, "SET check_function_bodies = off"); err != nil {
		return nil, err
	}
	pending := list
	for {
		var failed []Stmt
		progress := false
		for _, s := range pending {
			_, err := dst.PgConn().Exec(ctx, s.SQL).ReadAll()
			if err == nil || alreadyExists(err) {
				progress = true
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !s.Soft {
				return nil, fmt.Errorf("create %s %s: %s", s.Kind, s.Name, pgMessage(err))
			}
			s.err = err
			failed = append(failed, s)
		}
		if len(failed) == 0 || !progress {
			return failed, nil
		}
		pending = failed
	}
}

func reportFailed(rep Reporter, failed []Stmt) {
	for _, s := range failed {
		rep.Event("warn", fmt.Sprintf("could not create %s %s: %s", s.Kind, s.Name, pgMessage(s.err)))
	}
}
