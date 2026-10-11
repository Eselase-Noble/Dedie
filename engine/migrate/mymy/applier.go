package mymy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
)

// applier writes binary log changes to a MySQL target.
type applier struct {
	jobID string
	db    *sql.DB
	conn  *sql.Conn // one connection, so a transaction stays together
	wrote bool      // a target transaction is open
	// anomaly is called when a change did not hit exactly one row, which
	// means the target has drifted from the source.
	anomaly func(string)
}

func newApplier(ctx context.Context, dstCfg connector.Config, jobID string, anomaly func(string)) (*applier, error) {
	// Foreign-key checks stay on: InnoDB does not log the rows changed by a
	// cascading action, so the target has to carry out the cascade itself.
	db, err := mysql.Open(ctx, dstCfg, targetParams(nil), mysql.FoundRows)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, checkpointDDL); err != nil {
		conn.Close()
		db.Close()
		return nil, fmt.Errorf("create checkpoint table: %s", message(err))
	}
	return &applier{jobID: jobID, db: db, conn: conn, anomaly: anomaly}, nil
}

func (a *applier) Close() {
	a.conn.Close()
	a.db.Close()
}

func (a *applier) Checkpoint(ctx context.Context) (Position, error) {
	var file string
	var pos uint64
	err := a.conn.QueryRowContext(ctx, "SELECT binlog_file, binlog_pos FROM `"+checkpointTable+"` WHERE job_id = ?", a.jobID).Scan(&file, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		return Position{}, nil
	}
	return Position{File: file, Pos: uint32(pos)}, err
}

// begin opens the target transaction on the first change of a source one.
func (a *applier) begin(ctx context.Context) error {
	if a.wrote {
		return nil
	}
	if _, err := a.conn.ExecContext(ctx, "BEGIN"); err != nil {
		return errors.New(message(err))
	}
	a.wrote = true
	return nil
}

func (a *applier) Commit(ctx context.Context, pos Position) (bool, error) {
	if !a.wrote {
		return false, nil
	}
	if _, err := a.conn.ExecContext(ctx, checkpointUpsert, a.jobID, pos.File, uint64(pos.Pos)); err != nil {
		return false, fmt.Errorf("record checkpoint: %s", message(err))
	}
	if _, err := a.conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit on target: %s", message(err))
	}
	a.wrote = false
	return true, nil
}

func (a *applier) Rollback(ctx context.Context) {
	if a.wrote {
		a.conn.ExecContext(ctx, "ROLLBACK")
		a.wrote = false
	}
}

func (a *applier) Insert(ctx context.Context, t *Table, rows [][]any) error {
	if err := a.begin(ctx); err != nil {
		return err
	}
	names := t.writable()
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")"
	var args []any
	for _, row := range rows {
		for i, c := range t.Cols {
			if !c.Generated {
				args = append(args, binlogValue(c, row[i]))
			}
		}
	}
	stmt := "INSERT INTO " + t.QName() + " (" + strings.Join(names, ", ") + ") VALUES " + strings.TrimSuffix(strings.Repeat(tuple+",", len(rows)), ",")
	if _, err := a.conn.ExecContext(ctx, stmt, args...); err != nil {
		return errors.New(message(err))
	}
	return nil
}

func (a *applier) Update(ctx context.Context, t *Table, before, after []any) error {
	if err := a.begin(ctx); err != nil {
		return err
	}
	var sets []string
	var args []any
	for j, c := range t.Cols {
		if !c.Generated {
			sets = append(sets, mysql.Quote(c.Name)+" = ?")
			args = append(args, binlogValue(c, after[j]))
		}
	}
	where, args := t.where(before, args)
	res, err := a.conn.ExecContext(ctx, "UPDATE "+t.QName()+" SET "+strings.Join(sets, ", ")+" WHERE "+where, args...)
	if err != nil {
		return errors.New(message(err))
	}
	a.expectOne(res, "update", t)
	return nil
}

func (a *applier) Delete(ctx context.Context, t *Table, row []any) error {
	if err := a.begin(ctx); err != nil {
		return err
	}
	where, args := t.where(row, nil)
	res, err := a.conn.ExecContext(ctx, "DELETE FROM "+t.QName()+" WHERE "+where, args...)
	if err != nil {
		return errors.New(message(err))
	}
	a.expectOne(res, "delete", t)
	return nil
}

// Truncate empties the table. TRUNCATE commits implicitly in MySQL, on the
// source as on the target, so it is not part of a transaction.
func (a *applier) Truncate(ctx context.Context, t *Table) error {
	for _, stmt := range []string{"SET SESSION foreign_key_checks = 0", "TRUNCATE TABLE " + t.QName(), "SET SESSION foreign_key_checks = 1"} {
		if _, err := a.conn.ExecContext(ctx, stmt); err != nil {
			return errors.New(message(err))
		}
	}
	return nil
}

// where builds the row lookup from a before-image: the primary key when
// there is one, otherwise every column.
func (t *Table) where(row []any, args []any) (string, []any) {
	var conds []string
	add := func(c Column, v any) {
		name := mysql.Quote(c.Name)
		switch {
		case v == nil:
			conds = append(conds, name+" IS NULL")
		case c.DataType == "json":
			conds = append(conds, name+" = CAST(? AS JSON)")
			args = append(args, binlogValue(c, v))
		default:
			conds = append(conds, name+" = ?")
			args = append(args, binlogValue(c, v))
		}
	}
	if len(t.PK) > 0 {
		for _, i := range t.PK {
			add(t.Cols[i], row[i])
		}
		return strings.Join(conds, " AND "), args
	}
	for i, c := range t.Cols {
		if !c.Generated {
			add(c, row[i])
		}
	}
	// Identical rows are interchangeable; change exactly one of them.
	return strings.Join(conds, " AND ") + " LIMIT 1", args
}

func (a *applier) expectOne(res sql.Result, op string, t *Table) {
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		a.anomaly(fmt.Sprintf("%s on %s matched %d rows on the target (expected 1)", op, t.Name, n))
	}
}
