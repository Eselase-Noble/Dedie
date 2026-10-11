package pgmy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pglogrepl"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate/pgpg"
)

// applier writes PostgreSQL replication changes to a MySQL target.
type applier struct {
	jobID string
	plan  *Plan
	db    *sql.DB
	conn  *sql.Conn // one connection, so a transaction stays together
	wrote bool      // a target transaction is open
	// anomaly is called when a change did not hit exactly one row.
	anomaly func(string)
}

func newApplier(ctx context.Context, dstCfg connector.Config, jobID string, plan *Plan, anomaly func(string)) (*applier, error) {
	db, err := mysql.Open(ctx, dstCfg, targetParams, mysql.FoundRows)
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
	return &applier{jobID: jobID, plan: plan, db: db, conn: conn, anomaly: anomaly}, nil
}

func (a *applier) Close() {
	a.conn.Close()
	a.db.Close()
}

func (a *applier) Checkpoint(ctx context.Context) (pglogrepl.LSN, pglogrepl.LSN, error) {
	var commit, end string
	err := a.conn.QueryRowContext(ctx, "SELECT commit_lsn, end_lsn FROM `"+checkpointTable+"` WHERE job_id = ?", a.jobID).Scan(&commit, &end)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && commit == "") {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	c, _ := pglogrepl.ParseLSN(commit)
	e, _ := pglogrepl.ParseLSN(end)
	return c, e, nil
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

func (a *applier) Commit(ctx context.Context, commit, end pglogrepl.LSN) (bool, error) {
	if !a.wrote {
		return false, nil
	}
	if _, err := a.conn.ExecContext(ctx, checkpointUpsert, a.jobID, commit.String(), end.String()); err != nil {
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

// lookup finds the planned table for a relation in the stream.
func (a *applier) lookup(r *pgpg.Relation) (*Table, error) {
	t := a.plan.table(r.Schema, r.Name)
	if t == nil {
		return nil, fmt.Errorf("the stream carries a table that is not part of this migration")
	}
	return t, nil
}

// field is one column of a row image, matched to its translated column.
// Columns the target does not have (generated ones) are skipped, as are
// large values an UPDATE left untouched, which PostgreSQL does not resend.
type field struct {
	col   *Column
	key   bool
	value any // nil for NULL
}

func fields(t *Table, r *pgpg.Relation, tuple *pglogrepl.TupleData) []field {
	out := make([]field, 0, len(tuple.Columns))
	for i, c := range tuple.Columns {
		col := t.byName[r.Columns[i].Name]
		if col == nil || c.DataType == pglogrepl.TupleDataTypeToast {
			continue
		}
		f := field{col: col, key: r.Columns[i].Key}
		if c.DataType != pglogrepl.TupleDataTypeNull {
			f.value = convert(col, c.Data)
		}
		out = append(out, f)
	}
	return out
}

func (a *applier) Insert(ctx context.Context, r *pgpg.Relation, row *pglogrepl.TupleData) error {
	t, err := a.lookup(r)
	if err != nil {
		return err
	}
	if err := a.begin(ctx); err != nil {
		return err
	}
	fs := fields(t, r, row)
	names := make([]string, len(fs))
	args := make([]any, len(fs))
	for i, f := range fs {
		names[i], args[i] = mysql.Quote(f.col.Name), f.value
	}
	stmt := "INSERT INTO " + t.QName() + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(fs)), ",") + ")"
	if _, err := a.conn.ExecContext(ctx, stmt, args...); err != nil {
		return errors.New(message(err))
	}
	return nil
}

// where builds the row lookup from a key image: the replica identity
// columns, or every column when the source logs whole rows for the table.
func where(fs []field, full bool, args []any) (string, []any) {
	var conds []string
	for _, f := range fs {
		if !full && !f.key {
			continue
		}
		name := mysql.Quote(f.col.Name)
		switch {
		case f.value == nil:
			conds = append(conds, name+" IS NULL")
		case f.col.JSON:
			conds = append(conds, name+" = CAST(? AS JSON)")
			args = append(args, f.value)
		default:
			conds = append(conds, name+" = ?")
			args = append(args, f.value)
		}
	}
	clause := strings.Join(conds, " AND ")
	if full {
		// Identical rows are interchangeable; change exactly one of them.
		clause += " LIMIT 1"
	}
	return clause, args
}

func (a *applier) Update(ctx context.Context, r *pgpg.Relation, old *pglogrepl.TupleData, full bool, row *pglogrepl.TupleData) error {
	t, err := a.lookup(r)
	if err != nil {
		return err
	}
	if err := a.begin(ctx); err != nil {
		return err
	}
	fs := fields(t, r, row)
	if len(fs) == 0 {
		return nil
	}
	sets := make([]string, len(fs))
	args := make([]any, len(fs))
	for i, f := range fs {
		sets[i], args[i] = mysql.Quote(f.col.Name)+" = ?", f.value
	}
	ident := fs
	if old != nil {
		ident = fields(t, r, old)
	}
	clause, args := where(ident, full, args)
	res, err := a.conn.ExecContext(ctx, "UPDATE "+t.QName()+" SET "+strings.Join(sets, ", ")+" WHERE "+clause, args...)
	if err != nil {
		return errors.New(message(err))
	}
	a.expectOne(res, "update", t)
	return nil
}

func (a *applier) Delete(ctx context.Context, r *pgpg.Relation, old *pglogrepl.TupleData, full bool) error {
	t, err := a.lookup(r)
	if err != nil {
		return err
	}
	if err := a.begin(ctx); err != nil {
		return err
	}
	clause, args := where(fields(t, r, old), full, nil)
	res, err := a.conn.ExecContext(ctx, "DELETE FROM "+t.QName()+" WHERE "+clause, args...)
	if err != nil {
		return errors.New(message(err))
	}
	a.expectOne(res, "delete", t)
	return nil
}

// Truncate empties the tables. In MySQL TRUNCATE commits implicitly, so the
// rest of the source transaction is applied in a new target transaction;
// the checkpoint still moves only when the whole source transaction is done.
func (a *applier) Truncate(ctx context.Context, rels []*pgpg.Relation, restartIdentity, cascade bool) error {
	for _, r := range rels {
		t, err := a.lookup(r)
		if err != nil {
			return err
		}
		if _, err := a.conn.ExecContext(ctx, "TRUNCATE TABLE "+t.QName()); err != nil {
			return errors.New(message(err))
		}
	}
	// The implicit commit ended any open transaction.
	a.wrote = false
	return a.begin(ctx)
}

func (a *applier) expectOne(res sql.Result, op string, t *Table) {
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		a.anomaly(fmt.Sprintf("%s on %s matched %d rows on the target (expected 1)", op, t.Target, n))
	}
}

func message(err error) string {
	var myErr *driver.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Message
	}
	return err.Error()
}

// alreadyExists reports errors that mean an interrupted earlier attempt
// already created the object, which makes the DDL phases safe to re-run.
func alreadyExists(err error) bool {
	var myErr *driver.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1050, // table exists
		1061, // duplicate index name
		1826, // duplicate foreign key name
		1022: // duplicate key on write (constraint already there)
		return true
	}
	return false
}
