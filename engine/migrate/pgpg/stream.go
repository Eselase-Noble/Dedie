package pgpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

var errInterrupted = errors.New("interrupted by operator")

// Why Stream returned.
const (
	StopPause   = "pause"
	StopAbort   = "abort"
	StopCutover = "cutover"
)

// Relation is the stream's description of a table, sent by the source before
// the first change to it and again whenever its definition changes.
type Relation struct {
	Schema  string
	Name    string
	Columns []RelColumn
}

type RelColumn struct {
	Name    string
	TypeOID uint32
	// Key marks the columns that identify a row (the replica identity).
	Key bool
}

// Applier writes the changes decoded from the replication stream to a target
// database. The reader decides what happened and in which order; the applier
// knows how to express it for one kind of target. Values arrive in
// PostgreSQL's text form.
type Applier interface {
	// Checkpoint returns the commit position stored by the last commit and
	// the end of that transaction, or zeros when nothing was committed yet.
	Checkpoint(ctx context.Context) (commit, end pglogrepl.LSN, err error)
	Insert(ctx context.Context, r *Relation, row *pglogrepl.TupleData) error
	// Update receives the old row image when the source sent one: the key
	// columns only, or the whole row when full is set. Otherwise the key is
	// taken from the new image.
	Update(ctx context.Context, r *Relation, old *pglogrepl.TupleData, full bool, row *pglogrepl.TupleData) error
	Delete(ctx context.Context, r *Relation, old *pglogrepl.TupleData, full bool) error
	Truncate(ctx context.Context, rels []*Relation, restartIdentity, cascade bool) error
	// Commit ends the target transaction and stores the positions with it,
	// atomically. It reports false when the source transaction touched
	// nothing that is being migrated.
	Commit(ctx context.Context, commit, end pglogrepl.LSN) (bool, error)
	// Rollback discards an open target transaction.
	Rollback(ctx context.Context)
	Close()
}

// streamer reads the logical replication stream and hands it to an Applier.
type streamer struct {
	jobID  string
	cfg    Config
	st     *State
	rep    Reporter
	srcCfg connector.Config

	repl   *pgconn.PgConn // replication connection to the source
	mon    *pgx.Conn      // regular source connection: lag, fencing
	target Applier

	rels map[uint32]*Relation

	inTx   bool
	skipTx bool // transaction was applied before a restart

	checkpoint pglogrepl.LSN // commit LSN of the last applied transaction
	recvLSN    pglogrepl.LSN // everything before this is applied or irrelevant

	cutting     bool
	cutTarget   pglogrepl.LSN
	cutDeadline time.Time
}

// Stream replays the source's replication stream into target until the
// operator pauses, aborts or the cutover drain completes, and returns which
// of those happened.
func Stream(ctx context.Context, jobID string, cfg Config, st *State, srcCfg connector.Config, target Applier, rep Reporter) (string, error) {
	s := &streamer{jobID: jobID, cfg: cfg, st: st, rep: rep, srcCfg: srcCfg, target: target, rels: map[uint32]*Relation{}}
	bg := context.WithoutCancel(ctx)

	start, err := pglogrepl.ParseLSN(st.ConsistentLSN)
	if err != nil {
		return "", fmt.Errorf("bad consistent LSN %q: %w", st.ConsistentLSN, err)
	}
	commit, end, err := target.Checkpoint(ctx)
	if err != nil {
		return "", err
	}
	s.checkpoint = commit
	if end > start {
		start = end
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
	target.Rollback(bg)
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
				return StopPause, nil
			case "abort":
				return StopAbort, nil
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
			return StopCutover, nil
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

// handle decodes one pgoutput message and passes it on.
func (s *streamer) handle(ctx context.Context, data []byte) error {
	msg, err := pglogrepl.Parse(data)
	if err != nil {
		return fmt.Errorf("decode replication message: %w", err)
	}
	switch m := msg.(type) {
	case *pglogrepl.RelationMessage:
		r := &Relation{Schema: m.Namespace, Name: m.RelationName}
		for _, c := range m.Columns {
			r.Columns = append(r.Columns, RelColumn{Name: c.Name, TypeOID: c.DataType, Key: c.Flags&1 == 1})
		}
		s.rels[m.RelationID] = r

	case *pglogrepl.BeginMessage:
		s.inTx = true
		s.skipTx = m.FinalLSN <= s.checkpoint

	case *pglogrepl.InsertMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		if err := s.target.Insert(ctx, r, m.Tuple); err != nil {
			return fmt.Errorf("apply insert on %s.%s: %w", r.Schema, r.Name, err)
		}
		s.st.RowsApplied++

	case *pglogrepl.UpdateMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		full := m.OldTuple != nil && m.OldTupleType == pglogrepl.UpdateMessageTupleTypeOld
		if err := s.target.Update(ctx, r, m.OldTuple, full, m.NewTuple); err != nil {
			return fmt.Errorf("apply update on %s.%s: %w", r.Schema, r.Name, err)
		}
		s.st.RowsApplied++

	case *pglogrepl.DeleteMessage:
		r, err := s.rel(m.RelationID)
		if err != nil || s.skipTx {
			return err
		}
		if err := s.target.Delete(ctx, r, m.OldTuple, m.OldTupleType == pglogrepl.DeleteMessageTupleTypeOld); err != nil {
			return fmt.Errorf("apply delete on %s.%s: %w", r.Schema, r.Name, err)
		}
		s.st.RowsApplied++

	case *pglogrepl.TruncateMessage:
		if s.skipTx {
			return nil
		}
		var rels []*Relation
		for _, id := range m.RelationIDs {
			r, err := s.rel(id)
			if err != nil {
				return err
			}
			rels = append(rels, r)
		}
		err := s.target.Truncate(ctx, rels, m.Option&pglogrepl.TruncateOptionRestartIdentity != 0, m.Option&pglogrepl.TruncateOptionCascade != 0)
		if err != nil {
			return fmt.Errorf("apply truncate: %w", err)
		}

	case *pglogrepl.CommitMessage:
		if !s.skipTx {
			// The checkpoint commits atomically with the data, so after a
			// crash the target itself knows which transaction it applied last.
			applied, err := s.target.Commit(ctx, m.CommitLSN, m.TransactionEndLSN)
			if err != nil {
				return err
			}
			if applied {
				s.checkpoint = m.CommitLSN
				s.st.TxApplied++
				s.st.LastCommitTime = m.CommitTime
			}
		}
		s.inTx = false
		if m.TransactionEndLSN > s.recvLSN {
			s.recvLSN = m.TransactionEndLSN
		}
	}
	return nil
}

func (s *streamer) rel(id uint32) (*Relation, error) {
	r, ok := s.rels[id]
	if !ok {
		return nil, fmt.Errorf("replication stream referenced unknown relation %d", id)
	}
	return r, nil
}

// beginCutover optionally fences the source, then writes a marker into its
// WAL. Once the stream has passed the marker, every earlier commit has been
// applied.
func (s *streamer) beginCutover(ctx context.Context) error {
	if s.cfg.FenceSource && !s.st.Fenced {
		if err := Fence(ctx, s.mon); err != nil {
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
		if err := Unfence(ctx, s.srcCfg); err != nil {
			s.rep.Event("error", "could not make the source writable again: "+pgMessage(err))
		} else {
			s.st.Fenced = false
		}
	}
	s.rep.SetDesired("run")
	s.rep.SetPhase(PhaseStreaming)
	s.rep.Event("error", "cutover cancelled: "+why+"; streaming continues")
}
