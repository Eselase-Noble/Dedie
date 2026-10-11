package mypg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate/mymy"
)

// batchLimit caps the statements sent to the target in one round trip.
const batchLimit = 1000

// applier writes MySQL binary log changes to a PostgreSQL target. Statements
// of one source transaction are queued and sent in batches, and the
// transaction commits together with its binary log position.
type applier struct {
	jobID string
	plan  *Plan
	conn  *pgx.Conn
	pg    *pgconn.PgConn

	wrote  bool // a target transaction is open
	batch  *pgconn.Batch
	expect []string // per queued statement: non-empty if it must touch exactly one row
	// anomaly is called when a change did not hit exactly one row.
	anomaly func(string)
}

func newApplier(ctx context.Context, dstCfg connector.Config, jobID string, plan *Plan, anomaly func(string)) (*applier, error) {
	conn, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	if _, err := conn.PgConn().Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("create checkpoint table: %s", pgMessage(err))
	}
	return &applier{jobID: jobID, plan: plan, conn: conn, pg: conn.PgConn(), anomaly: anomaly}, nil
}

func (a *applier) Close() { a.conn.Close(context.Background()) }

func (a *applier) Checkpoint(ctx context.Context) (mymy.Position, error) {
	var file string
	var pos int64
	err := a.conn.QueryRow(ctx, `SELECT binlog_file, binlog_pos FROM `+metaSchema+`.mysql_checkpoint WHERE job_id = $1`, a.jobID).Scan(&file, &pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return mymy.Position{}, nil
	}
	return mymy.Position{File: file, Pos: uint32(pos)}, err
}

// queue adds a statement to the current target transaction, opening it on
// the first change. Parameters travel as text with no declared type, so
// PostgreSQL parses each value with the column's own input function.
func (a *applier) queue(ctx context.Context, sql string, args [][]byte, expectOne string) error {
	if a.batch == nil {
		a.batch = &pgconn.Batch{}
	}
	if !a.wrote {
		a.batch.ExecParams("BEGIN", nil, nil, nil, nil)
		a.expect = append(a.expect, "")
		a.wrote = true
	}
	a.batch.ExecParams(sql, args, nil, nil, nil)
	a.expect = append(a.expect, expectOne)
	if len(a.expect) >= batchLimit {
		return a.flush(ctx)
	}
	return nil
}

func (a *applier) flush(ctx context.Context) error {
	if a.batch == nil || len(a.expect) == 0 {
		return nil
	}
	results, err := a.pg.ExecBatch(ctx, a.batch).ReadAll()
	expect := a.expect
	a.batch, a.expect = nil, nil
	if err != nil {
		return errors.New(pgMessage(err))
	}
	for i, res := range results {
		if i < len(expect) && expect[i] != "" && res.CommandTag.RowsAffected() != 1 {
			a.anomaly(fmt.Sprintf("%s matched %d rows on the target (expected 1)", expect[i], res.CommandTag.RowsAffected()))
		}
	}
	return nil
}

func (a *applier) Commit(ctx context.Context, pos mymy.Position) (bool, error) {
	if !a.wrote {
		return false, nil
	}
	a.batch.ExecParams(checkpointUpsert, [][]byte{[]byte(a.jobID), []byte(pos.File), []byte(fmt.Sprint(pos.Pos))}, nil, nil, nil)
	a.batch.ExecParams("COMMIT", nil, nil, nil, nil)
	a.expect = append(a.expect, "", "")
	if err := a.flush(ctx); err != nil {
		return false, err
	}
	a.wrote = false
	return true, nil
}

func (a *applier) Rollback(ctx context.Context) {
	a.batch, a.expect = nil, nil
	if a.wrote {
		a.pg.Exec(ctx, "ROLLBACK").ReadAll()
		a.wrote = false
	}
}

// values converts one row image into text parameters.
func (a *applier) values(t *mymy.Table, row []any) [][]byte {
	enums := a.plan.enums[t.Name]
	out := make([][]byte, len(t.Cols))
	for i, c := range t.Cols {
		out[i] = streamValue(c, enums[i], row[i])
	}
	return out
}

func (a *applier) Insert(ctx context.Context, t *mymy.Table, rows [][]any) error {
	cols := make([]string, len(t.Cols))
	params := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		cols[i] = qident(c.Name)
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt := "INSERT INTO " + qname(t.Name) + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(params, ", ") + ")"
	for _, row := range rows {
		if err := a.queue(ctx, stmt, a.values(t, row), ""); err != nil {
			return err
		}
	}
	return nil
}

// where builds the row lookup from a before-image: the primary key when
// there is one. Without a key the row is found by all of its columns, and
// one physical row is picked so that duplicates are changed one at a time.
func (a *applier) where(t *mymy.Table, before [][]byte, args [][]byte) (string, [][]byte) {
	var conds []string
	add := func(i int) {
		if before[i] == nil {
			conds = append(conds, qident(t.Cols[i].Name)+" IS NULL")
			return
		}
		args = append(args, before[i])
		conds = append(conds, fmt.Sprintf("%s = $%d", qident(t.Cols[i].Name), len(args)))
	}
	if len(t.PK) > 0 {
		for _, i := range t.PK {
			add(i)
		}
		return strings.Join(conds, " AND "), args
	}
	for i := range t.Cols {
		add(i)
	}
	return "ctid = (SELECT ctid FROM " + qname(t.Name) + " WHERE " + strings.Join(conds, " AND ") + " LIMIT 1)", args
}

func (a *applier) Update(ctx context.Context, t *mymy.Table, before, after []any) error {
	args := a.values(t, after)
	sets := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		sets[i] = fmt.Sprintf("%s = $%d", qident(c.Name), i+1)
	}
	where, args := a.where(t, a.values(t, before), args)
	return a.queue(ctx, "UPDATE "+qname(t.Name)+" SET "+strings.Join(sets, ", ")+" WHERE "+where, args, "update on "+t.Name)
}

func (a *applier) Delete(ctx context.Context, t *mymy.Table, row []any) error {
	where, args := a.where(t, a.values(t, row), nil)
	return a.queue(ctx, "DELETE FROM "+qname(t.Name)+" WHERE "+where, args, "delete on "+t.Name)
}

// Truncate empties the table. MySQL commits implicitly at TRUNCATE, so it
// arrives outside any transaction; RESTART IDENTITY mirrors MySQL resetting
// the AUTO_INCREMENT counter.
func (a *applier) Truncate(ctx context.Context, t *mymy.Table) error {
	if _, err := a.pg.Exec(ctx, "TRUNCATE "+qname(t.Name)+" RESTART IDENTITY").ReadAll(); err != nil {
		return errors.New(pgMessage(err))
	}
	return nil
}

func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Detail != "" {
			return pgErr.Message + " (" + pgErr.Detail + ")"
		}
		return pgErr.Message
	}
	return err.Error()
}

// alreadyExists reports errors that mean an interrupted earlier attempt
// already created the object, which makes the DDL phases safe to re-run.
func alreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P07", "42710", "42P06", "42P16":
		return true
	}
	return false
}
