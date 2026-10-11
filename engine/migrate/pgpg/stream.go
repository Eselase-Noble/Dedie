package pgpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

var errInterrupted = errors.New("interrupted by operator")

// Why the stream loop returned.
const (
	stopPause   = "pause"
	stopAbort   = "abort"
	stopCutover = "cutover"
)

// batchLimit caps the statements sent to the target in one round trip.
const batchLimit = 1000

// relation is the stream's description of a table, sent by the source before
// the first change to it and again whenever its definition changes.
type relation struct {
	qname string
	cols  []string
	key   []bool
}

// streamer applies the logical replication stream to the target.
type streamer struct {
	jobID  string
	cfg    Config
	st     *State
	rep    Reporter
	srcCfg connector.Config

	repl *pgconn.PgConn // replication connection to the source
	dst  *pgconn.PgConn // apply connection to the target
	mon  *pgx.Conn      // regular source connection: lag, fencing

	rels map[uint32]*relation

	inTx      bool
	skipTx    bool // transaction was applied before a restart
	wrote     bool // target transaction is open
	batch     *pgconn.Batch
	expectOne []string // per queued statement: non-empty if it must touch exactly one row

	checkpoint pglogrepl.LSN // commit LSN of the last applied transaction
	recvLSN    pglogrepl.LSN // everything before this is applied or irrelevant

	cutting     bool
	cutTarget   pglogrepl.LSN
	cutDeadline time.Time
}

const checkpointDDL = `CREATE SCHEMA IF NOT EXISTS ` + metaSchema + `;
CREATE TABLE IF NOT EXISTS ` + metaSchema + `.checkpoint (
	job_id text PRIMARY KEY, commit_lsn pg_lsn NOT NULL, end_lsn pg_lsn NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

const checkpointUpsert = `INSERT INTO ` + metaSchema + `.checkpoint (job_id, commit_lsn, end_lsn) VALUES ($1, $2, $3)
	ON CONFLICT (job_id) DO UPDATE SET commit_lsn = excluded.commit_lsn, end_lsn = excluded.end_lsn, updated_at = now()`

// stream runs until the operator pauses, aborts or the cutover drain
// completes, and returns which of those happened.
func stream(ctx context.Context, jobID string, cfg Config, st *State, srcCfg, dstCfg connector.Config, rep Reporter) (string, error) {
	s := &streamer{jobID: jobID, cfg: cfg, st: st, rep: rep, srcCfg: srcCfg, rels: map[uint32]*relation{}}
	bg := context.WithoutCancel(ctx)

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer dst.Close(bg)
	s.dst = dst.PgConn()
	if _, err := s.dst.Exec(ctx, checkpointDDL).ReadAll(); err != nil {
		return "", fmt.Errorf("create checkpoint table: %w", err)
	}
	if st.ReplicaRole {
		if _, err := s.dst.Exec(ctx, "SET session_replication_role = replica").ReadAll(); err != nil {
			return "", fmt.Errorf("target no longer allows session_replication_role = replica: %s", pgMessage(err))
		}
	}

	start, err := pglogrepl.ParseLSN(st.ConsistentLSN)
	if err != nil {
		return "", fmt.Errorf("bad consistent LSN %q: %w", st.ConsistentLSN, err)
	}
	var commitLSN, endLSN string
	err = dst.QueryRow(ctx, `SELECT commit_lsn::text, end_lsn::text FROM `+metaSchema+`.checkpoint WHERE job_id=$1`, jobID).Scan(&commitLSN, &endLSN)
	if err == nil {
		s.checkpoint, _ = pglogrepl.ParseLSN(commitLSN)
		if end, perr := pglogrepl.ParseLSN(endLSN); perr == nil && end > start {
			start = end
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	s.recvLSN = start

	if s.mon, err = postgres.Connect(ctx, srcCfg); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer s.mon.Close(bg)

	if s.repl, err = postgres.ConnectReplication(ctx, srcCfg); err != nil {
		return "", fmt.Errorf("source replication connection: %w", err)
	}
	defer s.repl.Close(bg)
	err = pglogrepl.StartReplication(ctx, s.repl, st.Slot, start, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names '" + st.Publication + "'"},
	})
	if err != nil {
		return "", fmt.Errorf("start replication: %w", err)
	}

	reason, err := s.loop(ctx)
	if s.wrote {
		s.dst.Exec(bg, "ROLLBACK").ReadAll()
	}
	s.sendStatus(bg, false)
	s.saveProgress(bg)
	return reason, err
}

func (s *streamer) loop(ctx context.Context) (string, error) {
	var lastTick, lastStatus time.Time
	for {
		if time.Since(lastTick) >= time.Second {
			lastTick = time.Now()
			s.saveProgress(ctx)
			switch s.rep.Desired() {
			case "pause":
				return stopPause, nil
			case "abort":
				return stopAbort, nil
			case "cutover":
				if !s.cutting {
					if err := s.beginCutover(ctx); err != nil {
						return "", err
					}
				}
			}
			if s.cutting {
				if time.Now().After(s.cutDeadline) {
					s.cancelCutover(ctx, fmt.Sprintf("the stream did not drain within %d seconds", s.cfg.CutoverTimeoutSeconds))
				} else if err := s.sendStatus(ctx, true); err != nil {
					return "", err
				}
			}
		}
		if s.cutting && !s.inTx && s.recvLSN >= s.cutTarget {
			return stopCutover, nil
		}
		if time.Since(lastStatus) >= 5*time.Second {
			lastStatus = time.Now()
			if err := s.sendStatus(ctx, false); err != nil {
				return "", err
			}
		}

		rctx, cancel := context.WithTimeout(ctx, time.Second)
		raw, err := s.repl.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if pgconn.Timeout(err) {
				continue
			}
			return "", fmt.Errorf("replication stream: %w", err)
		}
		switch msg := raw.(type) {
		case *pgproto3.ErrorResponse:
			return "", fmt.Errorf("replication stream: %s", msg.Message)
		case *pgproto3.CopyData:
			switch msg.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				ka, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
				if err != nil {
					return "", err
				}
				// Outside a transaction nothing is pending, so the server's
				// position is safe to acknowledge; this lets it recycle WAL
				// even when none of the traffic touches migrated tables.
				if !s.inTx && ka.ServerWALEnd > s.recvLSN {
					s.recvLSN = ka.ServerWALEnd
				}
				if ka.ReplyRequested {
					if err := s.sendStatus(ctx, false); err != nil {
						return "", err
					}
				}
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
				if err != nil {
					return "", err
				}
				if err := s.handle(ctx, xld.WALData); err != nil {
					return "", err
				}
			}
		}
	}
}

func (s *streamer) sendStatus(ctx context.Context, requestReply bool) error {
	err := pglogrepl.SendStandbyStatusUpdate(ctx, s.repl, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: s.recvLSN, WALFlushPosition: s.recvLSN, WALApplyPosition: s.recvLSN,
		ReplyRequested: requestReply,
	})
	if err != nil {
		return fmt.Errorf("acknowledge position: %w", err)
	}
	return nil
}

// saveProgress refreshes lag figures and persists the state.
func (s *streamer) saveProgress(ctx context.Context) {
	var cur string
	if err := s.mon.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&cur); err == nil {
		s.st.SourceLSN = cur
		if lsn, err := pglogrepl.ParseLSN(cur); err == nil {
			s.st.LagBytes = 0
			if lsn > s.recvLSN {
				s.st.LagBytes = int64(lsn - s.recvLSN)
			}
		}
	}
	s.st.AppliedLSN = s.recvLSN.String()
	s.rep.SaveState(*s.st)
}

// handle decodes one pgoutput message and applies it.
func (s *streamer) handle(ctx context.Context, data []byte) error {
	msg, err := pglogrepl.Parse(data)
	if err != nil {
		return fmt.Errorf("decode replication message: %w", err)
	}
	switch m := msg.(type) {
	case *pglogrepl.RelationMessage:
		r := &relation{qname: qname(m.Namespace, m.RelationName)}
		for _, c := range m.Columns {
			r.cols = append(r.cols, qident(c.Name))
			r.key = append(r.key, c.Flags&1 == 1)
		}
		s.rels[m.RelationID] = r

	case *pglogrepl.BeginMessage:
		s.inTx, s.wrote = true, false
		s.skipTx = m.FinalLSN <= s.checkpoint

	case *pglogrepl.InsertMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		var cols, params []string
		var args [][]byte
		for i, c := range m.Tuple.Columns {
			if c.DataType == pglogrepl.TupleDataTypeToast {
				continue
			}
			cols = append(cols, r.cols[i])
			args = append(args, value(c))
			params = append(params, fmt.Sprintf("$%d", len(args)))
		}
		sql := "INSERT INTO " + r.qname + " (" + strings.Join(cols, ", ") + ") OVERRIDING SYSTEM VALUE VALUES (" + strings.Join(params, ", ") + ")"
		return s.queue(ctx, sql, args, "")

	case *pglogrepl.UpdateMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		var sets []string
		var args [][]byte
		for i, c := range m.NewTuple.Columns {
			// Large values the UPDATE did not touch are not resent.
			if c.DataType == pglogrepl.TupleDataTypeToast {
				continue
			}
			args = append(args, value(c))
			sets = append(sets, fmt.Sprintf("%s = $%d", r.cols[i], len(args)))
		}
		if len(sets) == 0 {
			return nil
		}
		ident, full := m.NewTuple, false
		if m.OldTuple != nil {
			ident, full = m.OldTuple, m.OldTupleType == pglogrepl.UpdateMessageTupleTypeOld
		}
		where, args := r.where(ident, full, args)
		return s.queue(ctx, "UPDATE "+r.qname+" SET "+strings.Join(sets, ", ")+" WHERE "+where, args, "update on "+r.qname)

	case *pglogrepl.DeleteMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		where, args := r.where(m.OldTuple, m.OldTupleType == pglogrepl.DeleteMessageTupleTypeOld, nil)
		return s.queue(ctx, "DELETE FROM "+r.qname+" WHERE "+where, args, "delete on "+r.qname)

	case *pglogrepl.TruncateMessage:
		if s.skipTx {
			return nil
		}
		var names []string
		for _, id := range m.RelationIDs {
			r, err := s.rel(id)
			if err != nil {
				return err
			}
			names = append(names, r.qname)
		}
		sql := "TRUNCATE " + strings.Join(names, ", ")
		if m.Option&pglogrepl.TruncateOptionRestartIdentity != 0 {
			sql += " RESTART IDENTITY"
		}
		if m.Option&pglogrepl.TruncateOptionCascade != 0 {
			sql += " CASCADE"
		}
		return s.queue(ctx, sql, nil, "")

	case *pglogrepl.CommitMessage:
		if s.wrote {
			// The checkpoint commits atomically with the data, so after a
			// crash the target itself knows which transaction it applied last.
			s.batch.ExecParams(checkpointUpsert, [][]byte{[]byte(s.jobID), []byte(m.CommitLSN.String()), []byte(m.TransactionEndLSN.String())}, nil, nil, nil)
			s.expectOne = append(s.expectOne, "")
			s.batch.ExecParams("COMMIT", nil, nil, nil, nil)
			s.expectOne = append(s.expectOne, "")
			if err := s.flush(ctx); err != nil {
				return err
			}
			s.wrote = false
			s.checkpoint = m.CommitLSN
			s.st.TxApplied++
			s.st.LastCommitTime = m.CommitTime
		}
		s.inTx = false
		if m.TransactionEndLSN > s.recvLSN {
			s.recvLSN = m.TransactionEndLSN
		}
	}
	return nil
}

func (s *streamer) rel(id uint32) (*relation, error) {
	r, ok := s.rels[id]
	if !ok {
		return nil, fmt.Errorf("replication stream referenced unknown relation %d", id)
	}
	return r, nil
}

func value(c *pglogrepl.TupleDataColumn) []byte {
	if c.DataType == pglogrepl.TupleDataTypeNull {
		return nil
	}
	return c.Data
}

// where builds the row lookup from a key tuple. With REPLICA IDENTITY FULL
// there is no key, so the row is matched on every column of its old image.
func (r *relation) where(t *pglogrepl.TupleData, full bool, args [][]byte) (string, [][]byte) {
	var conds []string
	for i, c := range t.Columns {
		if !full && !r.key[i] {
			continue
		}
		switch c.DataType {
		case pglogrepl.TupleDataTypeToast:
			continue
		case pglogrepl.TupleDataTypeNull:
			conds = append(conds, r.cols[i]+" IS NULL")
		default:
			args = append(args, c.Data)
			conds = append(conds, fmt.Sprintf("%s = $%d", r.cols[i], len(args)))
		}
	}
	return strings.Join(conds, " AND "), args
}

// queue adds a statement to the current target transaction, opening it on
// the first change. Parameters travel in text form with no declared type, so
// the target parses each value with the column's own input function.
func (s *streamer) queue(ctx context.Context, sql string, args [][]byte, expectOne string) error {
	if s.batch == nil {
		s.batch = &pgconn.Batch{}
	}
	if !s.wrote {
		s.batch.ExecParams("BEGIN", nil, nil, nil, nil)
		s.expectOne = append(s.expectOne, "")
		s.wrote = true
	}
	s.batch.ExecParams(sql, args, nil, nil, nil)
	s.expectOne = append(s.expectOne, expectOne)
	s.st.RowsApplied++
	if len(s.expectOne) >= batchLimit {
		return s.flush(ctx)
	}
	return nil
}

func (s *streamer) flush(ctx context.Context) error {
	if s.batch == nil || len(s.expectOne) == 0 {
		return nil
	}
	results, err := s.dst.ExecBatch(ctx, s.batch).ReadAll()
	expect := s.expectOne
	s.batch, s.expectOne = nil, nil
	if err != nil {
		return fmt.Errorf("apply change on target: %s", pgMessage(err))
	}
	// An UPDATE or DELETE that does not hit exactly one row means the target
	// has drifted from the source (for example someone wrote to it directly).
	for i, res := range results {
		if i < len(expect) && expect[i] != "" && res.CommandTag.RowsAffected() != 1 {
			s.st.Anomalies++
			if s.st.Anomalies <= 20 {
				s.rep.Event("warn", fmt.Sprintf("%s matched %d rows on the target (expected 1)", expect[i], res.CommandTag.RowsAffected()))
			}
		}
	}
	return nil
}

// beginCutover optionally fences the source, then writes a marker into its
// WAL. Once the stream has passed the marker, every earlier commit has been
// applied.
func (s *streamer) beginCutover(ctx context.Context) error {
	if s.cfg.FenceSource && !s.st.Fenced {
		if err := fence(ctx, s.mon); err != nil {
			s.rep.Event("error", "cutover cancelled: could not make the source read-only: "+pgMessage(err))
			s.rep.SetDesired("run")
			return nil
		}
		s.st.Fenced = true
		s.rep.SaveState(*s.st)
		s.rep.Event("info", "source database is now read-only and its clients were disconnected")
	}
	var marker string
	err := s.mon.QueryRow(ctx, `SELECT pg_logical_emit_message(false, 'dedie', 'cutover')::text`).Scan(&marker)
	if err != nil {
		return fmt.Errorf("write cutover marker: %w", err)
	}
	if s.cutTarget, err = pglogrepl.ParseLSN(marker); err != nil {
		return err
	}
	s.cutting = true
	s.cutDeadline = time.Now().Add(time.Duration(s.cfg.CutoverTimeoutSeconds) * time.Second)
	s.rep.SetPhase(PhaseCutover)
	s.rep.Event("info", "cutover started: draining the replication stream up to "+marker)
	return nil
}

func (s *streamer) cancelCutover(ctx context.Context, why string) {
	s.cutting = false
	if s.st.Fenced {
		if err := unfence(ctx, s.srcCfg); err != nil {
			s.rep.Event("error", "could not make the source writable again: "+pgMessage(err))
		} else {
			s.st.Fenced = false
		}
	}
	s.rep.SetDesired("run")
	s.rep.SetPhase(PhaseStreaming)
	s.rep.Event("error", "cutover cancelled: "+why+"; streaming continues")
}
