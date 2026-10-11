package mymy

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

// Why Stream returned.
const (
	StopPause   = "pause"
	StopAbort   = "abort"
	StopCutover = "cutover"
)

// insertChunk caps the rows of one row event sent in a single INSERT.
const insertChunk = 200

var (
	truncateRe = regexp.MustCompile("(?i)^TRUNCATE\\s+(?:TABLE\\s+)?(?:`[^`]+`\\.)?`?([^`\\s;]+)`?")
	ddlRe      = regexp.MustCompile(`(?i)^(CREATE|ALTER|DROP|RENAME)\s+(TABLE|INDEX|UNIQUE|FULLTEXT|SPATIAL)`)
)

// streamer applies the source's binary log to the target.
type streamer struct {
	jobID  string
	cfg    migrate.Config
	st     *State
	rep    migrate.Reporter
	plan   *Plan
	srcCfg connector.Config

	mon    *sql.DB // source: binary log position, fencing
	target Applier

	pos  Position // end of the last event read
	inTx bool     // inside a source transaction

	cutting     bool
	cutTarget   Position
	cutDeadline time.Time
}

// Applier writes the changes read from the binary log to a target database.
// The reader decides what happened and in which order; the applier knows how
// to express it for one kind of target. Row values arrive as decoded by the
// binary log reader and are described by the table's columns.
type Applier interface {
	// Checkpoint returns the position stored by the last commit, or a zero
	// Position when the job has not committed anything yet.
	Checkpoint(ctx context.Context) (Position, error)
	Insert(ctx context.Context, t *Table, rows [][]any) error
	Update(ctx context.Context, t *Table, before, after []any) error
	Delete(ctx context.Context, t *Table, row []any) error
	Truncate(ctx context.Context, t *Table) error
	// Commit ends the target transaction and stores pos with it, atomically.
	// It reports false when the source transaction touched nothing that is
	// being migrated, in which case there was nothing to commit.
	Commit(ctx context.Context, pos Position) (bool, error)
	// Rollback discards an open target transaction.
	Rollback(ctx context.Context)
	Close()
}

// Stream replays the source's binary log into target until the operator
// pauses, aborts or the cutover drain completes, and returns which of those
// happened.
func Stream(ctx context.Context, jobID string, cfg migrate.Config, st *State, plan *Plan, srcCfg connector.Config, target Applier, rep migrate.Reporter) (string, error) {
	s := &streamer{jobID: jobID, cfg: cfg, st: st, rep: rep, plan: plan, srcCfg: srcCfg, target: target}
	bg := context.WithoutCancel(ctx)

	// Resume from the position the target itself recorded, if any.
	s.pos = st.Start
	saved, err := target.Checkpoint(ctx)
	if err != nil {
		return "", err
	}
	if saved.File != "" {
		s.pos = saved
	}

	if s.mon, err = mysql.Open(ctx, srcCfg, nil); err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer s.mon.Close()

	tlsCfg, err := mysql.TLSConfig(srcCfg)
	if err != nil {
		return "", err
	}
	if tlsCfg == nil && srcCfg.SSLMode != "disable" && mysql.Encrypted(ctx, s.mon) {
		// "prefer": the server offers TLS, so the replication connection uses it too.
		tlsCfg = &tls.Config{InsecureSkipVerify: true}
	}
	port := srcCfg.Port
	if port == 0 {
		port = 3306
	}
	syncer := replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
		// Replication clients need an id no other client of the source uses.
		ServerID: 1<<30 + rand.Uint32N(1<<30),
		Flavor:   "mysql",
		Host:     srcCfg.Host, Port: uint16(port), User: srcCfg.Username, Password: srcCfg.Password,
		Charset:                 "utf8mb4",
		TLSConfig:               tlsCfg,
		TimestampStringLocation: time.UTC,
		HeartbeatPeriod:         10 * time.Second,
		ReadTimeout:             90 * time.Second,
		DisableRetrySync:        true,
		Logger:                  slog.New(slog.DiscardHandler),
	})
	defer syncer.Close()
	events, err := syncer.StartSync(gomysql.Position{Name: s.pos.File, Pos: s.pos.Pos})
	if err != nil {
		return "", fmt.Errorf("start replication: %w", err)
	}

	reason, err := s.loop(ctx, events)
	target.Rollback(bg)
	s.saveProgress(bg)
	return reason, err
}

func (s *streamer) loop(ctx context.Context, events *replication.BinlogStreamer) (string, error) {
	var lastTick time.Time
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
			if s.cutting && time.Now().After(s.cutDeadline) {
				s.cancelCutover(ctx, fmt.Sprintf("the stream did not drain within %d seconds", s.cfg.CutoverTimeoutSeconds))
			}
		}
		if s.cutting && !s.inTx && s.pos.reached(s.cutTarget) {
			return StopCutover, nil
		}

		rctx, cancel := context.WithTimeout(ctx, time.Second)
		ev, err := events.GetEvent(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return "", fmt.Errorf("replication stream: %w", err)
		}
		if err := s.handle(ctx, ev); err != nil {
			return "", err
		}
	}
}

// saveProgress refreshes the lag figure and persists the state.
func (s *streamer) saveProgress(ctx context.Context) {
	if cur, err := BinlogStatus(ctx, s.mon); err == nil {
		s.st.SourceLSN = cur.String()
		s.st.LagBytes = s.lag(ctx, cur)
	}
	s.st.Applied = s.pos
	s.st.AppliedLSN = s.pos.String()
	s.rep.SaveState(*s.st)
}

// lag is the number of binary log bytes between the stream and the source.
func (s *streamer) lag(ctx context.Context, cur Position) int64 {
	if cur.File == s.pos.File {
		if cur.Pos > s.pos.Pos {
			return int64(cur.Pos - s.pos.Pos)
		}
		return 0
	}
	// The source has moved on to later files: add up what lies between.
	logs, err := QueryStrings(ctx, s.mon, "SHOW BINARY LOGS")
	if err != nil {
		return 0
	}
	var total int64
	counting := false
	for _, l := range logs {
		if len(l) < 2 {
			continue
		}
		var size int64
		fmt.Sscan(l[1], &size)
		switch {
		case l[0] == s.pos.File:
			counting = true
			total += size - int64(s.pos.Pos)
		case l[0] == cur.File:
			return total + int64(cur.Pos)
		case counting:
			total += size
		}
	}
	return total
}

func (s *streamer) handle(ctx context.Context, ev *replication.BinlogEvent) error {
	h := ev.Header
	switch e := ev.Event.(type) {
	case *replication.RotateEvent:
		s.pos = Position{File: string(e.NextLogName), Pos: uint32(e.Position)}
		return nil

	case *replication.QueryEvent:
		query := strings.TrimSpace(string(e.Query))
		switch {
		case strings.EqualFold(query, "BEGIN"):
			s.inTx = true
		case strings.EqualFold(query, "COMMIT"):
			s.advance(h)
			return s.commit(ctx, h)
		case string(e.Schema) == s.plan.Database:
			if err := s.schemaChange(ctx, query); err != nil {
				return err
			}
		}

	case *replication.RowsEvent:
		if string(e.Table.Schema) != s.plan.Database {
			break
		}
		t := s.plan.byName[string(e.Table.Table)]
		if t == nil {
			break
		}
		if err := s.applyRows(ctx, t, h.EventType, e); err != nil {
			return err
		}

	case *replication.XIDEvent:
		s.advance(h)
		return s.commit(ctx, h)
	}
	s.advance(h)
	return nil
}

// advance moves the stream position past a real event. Heartbeats and the
// synthetic events sent at connection start carry no position.
func (s *streamer) advance(h *replication.EventHeader) {
	const artificial = 0x20
	if h.LogPos > 0 && h.Flags&artificial == 0 && h.EventType != replication.HEARTBEAT_EVENT {
		s.pos.Pos = h.LogPos
	}
}

// commit ends the target transaction together with the checkpoint, so after
// a crash the target itself knows which source transaction it applied last.
func (s *streamer) commit(ctx context.Context, h *replication.EventHeader) error {
	s.inTx = false
	applied, err := s.target.Commit(ctx, s.pos)
	if err != nil {
		return err
	}
	if applied {
		s.st.TxApplied++
		s.st.LastCommitTime = time.Unix(int64(h.Timestamp), 0).UTC()
	}
	return nil
}

func (s *streamer) applyRows(ctx context.Context, t *Table, typ replication.EventType, e *replication.RowsEvent) error {
	if int(e.ColumnCount) != len(t.Cols) {
		return fmt.Errorf("table %s has %d columns in the binary log but had %d when the migration started; "+
			"schema changes on the source are not replicated", t.Name, e.ColumnCount, len(t.Cols))
	}
	switch typ {
	case replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		for start := 0; start < len(e.Rows); start += insertChunk {
			chunk := e.Rows[start:min(start+insertChunk, len(e.Rows))]
			if err := s.target.Insert(ctx, t, chunk); err != nil {
				return fmt.Errorf("apply insert on %s: %w", t.Name, err)
			}
			s.st.RowsApplied += int64(len(chunk))
		}

	case replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		// Rows come in pairs: the image before the change, then after.
		for i := 0; i+1 < len(e.Rows); i += 2 {
			if err := s.target.Update(ctx, t, e.Rows[i], e.Rows[i+1]); err != nil {
				return fmt.Errorf("apply update on %s: %w", t.Name, err)
			}
			s.st.RowsApplied++
		}

	case replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		for _, row := range e.Rows {
			if err := s.target.Delete(ctx, t, row); err != nil {
				return fmt.Errorf("apply delete on %s: %w", t.Name, err)
			}
			s.st.RowsApplied++
		}

	default:
		return fmt.Errorf("unsupported row event %s on table %s; if binlog_row_value_options is set to PARTIAL_JSON on the source, unset it", typ, t.Name)
	}
	return nil
}

// schemaChange handles statements logged for the migrated database.
// TRUNCATE is replayed. Other DDL is not replicated and is reported, because
// from then on the two schemas differ.
func (s *streamer) schemaChange(ctx context.Context, query string) error {
	if m := truncateRe.FindStringSubmatch(query); m != nil {
		t := s.plan.byName[m[1]]
		if t == nil {
			return nil
		}
		if err := s.target.Truncate(ctx, t); err != nil {
			return fmt.Errorf("apply truncate on %s: %w", t.Name, err)
		}
		return nil
	}
	if ddlRe.MatchString(query) {
		s.st.Anomalies++
		if len(query) > 200 {
			query = query[:200] + "..."
		}
		s.rep.Event("warn", "schema change on the source was not replicated: "+query)
	}
	return nil
}

// beginCutover optionally fences the source, then notes the end of its
// binary log. Once the stream has read up to that point, every earlier
// commit has been applied.
func (s *streamer) beginCutover(ctx context.Context) error {
	if s.cfg.FenceSource && !s.st.Fenced {
		if err := Fence(ctx, s.mon, s.plan.Database); err != nil {
			s.rep.Event("error", "cutover cancelled: could not make the source read-only: "+message(err))
			s.rep.SetDesired("run")
			return nil
		}
		s.st.Fenced = true
		s.rep.SaveState(*s.st)
		s.rep.Event("info", "source database is now read-only")
	}
	target, err := BinlogStatus(ctx, s.mon)
	if err != nil {
		return fmt.Errorf("read the source's binary log position: %s", message(err))
	}
	s.cutTarget, s.cutting = target, true
	s.cutDeadline = time.Now().Add(time.Duration(s.cfg.CutoverTimeoutSeconds) * time.Second)
	s.rep.SetPhase(migrate.PhaseCutover)
	s.rep.Event("info", "cutover started: draining the binary log up to "+target.String())
	return nil
}

func (s *streamer) cancelCutover(ctx context.Context, why string) {
	s.cutting = false
	if s.st.Fenced {
		if err := Unfence(ctx, s.srcCfg); err != nil {
			s.rep.Event("error", "could not make the source writable again: "+message(err))
		} else {
			s.st.Fenced = false
		}
	}
	s.rep.SetDesired("run")
	s.rep.SetPhase(migrate.PhaseStreaming)
	s.rep.Event("error", "cutover cancelled: "+why+"; streaming continues")
}
