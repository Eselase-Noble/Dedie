package pgpg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// batchLimit caps the statements sent to the target in one round trip.
const batchLimit = 1000

const checkpointDDL = `CREATE SCHEMA IF NOT EXISTS ` + metaSchema + `;
CREATE TABLE IF NOT EXISTS ` + metaSchema + `.checkpoint (
	job_id text PRIMARY KEY, commit_lsn pg_lsn NOT NULL, end_lsn pg_lsn NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

const checkpointUpsert = `INSERT INTO ` + metaSchema + `.checkpoint (job_id, commit_lsn, end_lsn) VALUES ($1, $2, $3)
	ON CONFLICT (job_id) DO UPDATE SET commit_lsn = excluded.commit_lsn, end_lsn = excluded.end_lsn, updated_at = now()`

// applier writes replicated changes to a PostgreSQL target. Statements of
// one source transaction are queued and sent in batches.
type applier struct {
	jobID string
	conn  *pgx.Conn
	pg    *pgconn.PgConn

	wrote  bool // a target transaction is open
	batch  *pgconn.Batch
	expect []string // per queued statement: non-empty if it must touch exactly one row
	// anomaly is called when a change did not hit exactly one row, which
	// means the target has drifted from the source.
	anomaly func(string)
}

func newApplier(ctx context.Context, dstCfg connector.Config, jobID string, replicaRole bool, anomaly func(string)) (*applier, error) {
	conn, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	a := &applier{jobID: jobID, conn: conn, pg: conn.PgConn(), anomaly: anomaly}
	if _, err := a.pg.Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		a.Close()
		return nil, fmt.Errorf("create checkpoint table: %w", err)
	}
	if replicaRole {
		if _, err := a.pg.Exec(ctx, "SET session_replication_role = replica").ReadAll(); err != nil {
			a.Close()
			return nil, fmt.Errorf("target no longer allows session_replication_role = replica: %s", pgMessage(err))
		}
	}
	return a, nil
}

func (a *applier) Close() { a.conn.Close(context.Background()) }

func (a *applier) Checkpoint(ctx context.Context) (pglogrepl.LSN, pglogrepl.LSN, error) {
	var commit, end string
	err := a.conn.QueryRow(ctx, `SELECT commit_lsn::text, end_lsn::text FROM `+metaSchema+`.checkpoint WHERE job_id=$1`, a.jobID).Scan(&commit, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	c, _ := pglogrepl.ParseLSN(commit)
	e, _ := pglogrepl.ParseLSN(end)
	return c, e, nil
}

func value(c *pglogrepl.TupleDataColumn) []byte {
	if c.DataType == pglogrepl.TupleDataTypeNull {
		return nil
	}
	return c.Data
}

func relName(r *Relation) string { return qname(r.Schema, r.Name) }

func (a *applier) Insert(ctx context.Context, r *Relation, row *pglogrepl.TupleData) error {
	var cols, params []string
	var args [][]byte
	for i, c := range row.Columns {
		if c.DataType == pglogrepl.TupleDataTypeToast {
			continue
		}
		cols = append(cols, qident(r.Columns[i].Name))
		args = append(args, value(c))
		params = append(params, fmt.Sprintf("$%d", len(args)))
	}
	sql := "INSERT INTO " + relName(r) + " (" + strings.Join(cols, ", ") + ") OVERRIDING SYSTEM VALUE VALUES (" + strings.Join(params, ", ") + ")"
	return a.queue(ctx, sql, args, "")
}

func (a *applier) Update(ctx context.Context, r *Relation, old *pglogrepl.TupleData, full bool, row *pglogrepl.TupleData) error {
	var sets []string
	var args [][]byte
	for i, c := range row.Columns {
		// Large values the UPDATE did not touch are not resent.
		if c.DataType == pglogrepl.TupleDataTypeToast {
			continue
		}
		args = append(args, value(c))
		sets = append(sets, fmt.Sprintf("%s = $%d", qident(r.Columns[i].Name), len(args)))
	}
	if len(sets) == 0 {
		return nil
	}
	ident := row
	if old != nil {
		ident = old
	}
	where, args := where(r, ident, full, args)
	return a.queue(ctx, "UPDATE "+relName(r)+" SET "+strings.Join(sets, ", ")+" WHERE "+where, args, "update on "+relName(r))
}

func (a *applier) Delete(ctx context.Context, r *Relation, old *pglogrepl.TupleData, full bool) error {
	where, args := where(r, old, full, nil)
	return a.queue(ctx, "DELETE FROM "+relName(r)+" WHERE "+where, args, "delete on "+relName(r))
}

func (a *applier) Truncate(ctx context.Context, rels []*Relation, restartIdentity, cascade bool) error {
	names := make([]string, len(rels))
	for i, r := range rels {
		names[i] = relName(r)
	}
	sql := "TRUNCATE " + strings.Join(names, ", ")
	if restartIdentity {
		sql += " RESTART IDENTITY"
	}
	if cascade {
		sql += " CASCADE"
	}
	return a.queue(ctx, sql, nil, "")
}

// where builds the row lookup from a key tuple. With REPLICA IDENTITY FULL
// there is no key, so the row is matched on every column of its old image.
func where(r *Relation, t *pglogrepl.TupleData, full bool, args [][]byte) (string, [][]byte) {
	var conds []string
	for i, c := range t.Columns {
		if !full && !r.Columns[i].Key {
			continue
		}
		name := qident(r.Columns[i].Name)
		switch c.DataType {
		case pglogrepl.TupleDataTypeToast:
			continue
		case pglogrepl.TupleDataTypeNull:
			conds = append(conds, name+" IS NULL")
		default:
			args = append(args, c.Data)
			conds = append(conds, fmt.Sprintf("%s = $%d", name, len(args)))
		}
	}
	return strings.Join(conds, " AND "), args
}

// queue adds a statement to the current target transaction, opening it on
// the first change. Parameters travel in text form with no declared type, so
// the target parses each value with the column's own input function.
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
	// An UPDATE or DELETE that does not hit exactly one row means the target
	// has drifted from the source (for example someone wrote to it directly).
	for i, res := range results {
		if i < len(expect) && expect[i] != "" && res.CommandTag.RowsAffected() != 1 {
			a.anomaly(fmt.Sprintf("%s matched %d rows on the target (expected 1)", expect[i], res.CommandTag.RowsAffected()))
		}
	}
	return nil
}

func (a *applier) Commit(ctx context.Context, commit, end pglogrepl.LSN) (bool, error) {
	if !a.wrote {
		return false, nil
	}
	a.batch.ExecParams(checkpointUpsert, [][]byte{[]byte(a.jobID), []byte(commit.String()), []byte(end.String())}, nil, nil, nil)
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
