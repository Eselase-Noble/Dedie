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

// Why the stream loop returned.
const (
	stopPause   = "pause"
	stopAbort   = "abort"
	stopCutover = "cutover"
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

	mon *sql.DB   // source: binary log position, fencing
	dst *sql.Conn // target: one connection, so transactions stay together

	pos   Position // end of the last event read
	inTx  bool     // inside a source transaction
	wrote bool     // a target transaction is open

	cutting     bool
	cutTarget   Position
	cutDeadline time.Time
}

// stream runs until the operator pauses, aborts or the cutover drain
// completes, and returns which of those happened.
func stream(ctx context.Context, jobID string, cfg migrate.Config, st *State, plan *Plan, srcCfg, dstCfg connector.Config, rep migrate.Reporter) (string, error) {
	s := &streamer{jobID: jobID, cfg: cfg, st: st, rep: rep, plan: plan, srcCfg: srcCfg}
	bg := context.WithoutCancel(ctx)

	// Foreign-key checks stay on: InnoDB does not log the rows changed by a
	// cascading action, so the target has to carry out the cascade itself.
	dstDB, err := mysql.Open(ctx, dstCfg, targetParams(nil), mysql.FoundRows)
	if err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer dstDB.Close()
	if s.dst, err = dstDB.Conn(ctx); err != nil {
		return "", err
	}
	defer s.dst.Close()
	if _, err := s.dst.ExecContext(ctx, checkpointDDL); err != nil {
		return "", fmt.Errorf("create checkpoint table: %s", message(err))
	}

	// Resume from the position the target itself recorded, if any.
	s.pos = st.Start
	var file string
	var pos uint64
	err = s.dst.QueryRowContext(ctx, "SELECT binlog_file, binlog_pos FROM `"+checkpointTable+"` WHERE job_id = ?", jobID).Scan(&file, &pos)
	if err == nil && file != "" {
		s.pos = Position{File: file, Pos: uint32(pos)}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
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
	if s.wrote {
		s.dst.ExecContext(bg, "ROLLBACK")
	}
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
			if s.cutting && time.Now().After(s.cutDeadline) {
				s.cancelCutover(ctx, fmt.Sprintf("the stream did not drain within %d seconds", s.cfg.CutoverTimeoutSeconds))
			}
		}
		if s.cutting && !s.inTx && s.pos.reached(s.cutTarget) {
			return stopCutover, nil
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
	if cur, err := binlogStatus(ctx, s.mon); err == nil {
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
	logs, err := queryStrings(ctx, s.mon, "SHOW BINARY LOGS")
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

func (s *streamer) begin(ctx context.Context) error {
	if s.wrote {
		return nil
	}
	if _, err := s.dst.ExecContext(ctx, "BEGIN"); err != nil {
		return fmt.Errorf("begin target transaction: %s", message(err))
	}
	s.wrote = true
	return nil
}

// commit ends the target transaction together with the checkpoint, so after
// a crash the target itself knows which source transaction it applied last.
func (s *streamer) commit(ctx context.Context, h *replication.EventHeader) error {
	s.inTx = false
	if !s.wrote {
		return nil
	}
	if _, err := s.dst.ExecContext(ctx, checkpointUpsert, s.jobID, s.pos.File, uint64(s.pos.Pos)); err != nil {
		return fmt.Errorf("record checkpoint: %s", message(err))
	}
	if _, err := s.dst.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit on target: %s", message(err))
	}
	s.wrote = false
	s.st.TxApplied++
	s.st.LastCommitTime = time.Unix(int64(h.Timestamp), 0).UTC()
	return nil
}

func (s *streamer) applyRows(ctx context.Context, t *Table, typ replication.EventType, e *replication.RowsEvent) error {
	if int(e.ColumnCount) != len(t.Cols) {
		return fmt.Errorf("table %s has %d columns in the binary log but had %d when the migration started; "+
			"schema changes on the source are not replicated", t.Name, e.ColumnCount, len(t.Cols))
	}
	if err := s.begin(ctx); err != nil {
		return err
	}
	switch typ {
	case replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		names := strings.Join(t.writable(), ", ")
		tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(t.writable())), ",") + ")"
		for start := 0; start < len(e.Rows); start += insertChunk {
			chunk := e.Rows[start:min(start+insertChunk, len(e.Rows))]
			var args []any
			for _, row := range chunk {
				for i, c := range t.Cols {
					if !c.Generated {
						args = append(args, binlogValue(c, row[i]))
					}
				}
			}
			stmt := "INSERT INTO " + t.QName() + " (" + names + ") VALUES " + strings.TrimSuffix(strings.Repeat(tuple+",", len(chunk)), ",")
			if _, err := s.dst.ExecContext(ctx, stmt, args...); err != nil {
				return fmt.Errorf("apply insert on %s: %s", t.Name, message(err))
			}
			s.st.RowsApplied += int64(len(chunk))
		}

	case replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		// Rows come in pairs: the image before the change, then after.
		for i := 0; i+1 < len(e.Rows); i += 2 {
			before, after := e.Rows[i], e.Rows[i+1]
			var sets []string
			var args []any
			for j, c := range t.Cols {
				if !c.Generated {
					sets = append(sets, mysql.Quote(c.Name)+" = ?")
					args = append(args, binlogValue(c, after[j]))
				}
			}
			where, args := t.where(before, args)
			res, err := s.dst.ExecContext(ctx, "UPDATE "+t.QName()+" SET "+strings.Join(sets, ", ")+" WHERE "+where, args...)
			if err != nil {
				return fmt.Errorf("apply update on %s: %s", t.Name, message(err))
			}
			s.expectOne(res, "update", t)
		}

	case replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		for _, row := range e.Rows {
			where, args := t.where(row, nil)
			res, err := s.dst.ExecContext(ctx, "DELETE FROM "+t.QName()+" WHERE "+where, args...)
			if err != nil {
				return fmt.Errorf("apply delete on %s: %s", t.Name, message(err))
			}
			s.expectOne(res, "delete", t)
		}

	default:
		return fmt.Errorf("unsupported row event %s on table %s; if binlog_row_value_options is set to PARTIAL_JSON on the source, unset it", typ, t.Name)
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

// expectOne counts statements that did not hit exactly one row. That means
// the target has drifted from the source, for example because something
// else wrote to it.
func (s *streamer) expectOne(res sql.Result, op string, t *Table) {
	s.st.RowsApplied++
	n, err := res.RowsAffected()
	if err != nil || n == 1 {
		return
	}
	s.st.Anomalies++
	if s.st.Anomalies <= 20 {
		s.rep.Event("warn", fmt.Sprintf("%s on %s matched %d rows on the target (expected 1)", op, t.Name, n))
	}
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
		for _, stmt := range []string{"SET SESSION foreign_key_checks = 0", "TRUNCATE TABLE " + t.QName(), "SET SESSION foreign_key_checks = 1"} {
			if _, err := s.dst.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply truncate on %s: %s", t.Name, message(err))
			}
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
		if err := fence(ctx, s.mon, s.plan.Database); err != nil {
			s.rep.Event("error", "cutover cancelled: could not make the source read-only: "+message(err))
			s.rep.SetDesired("run")
			return nil
		}
		s.st.Fenced = true
		s.rep.SaveState(*s.st)
		s.rep.Event("info", "source database is now read-only")
	}
	target, err := binlogStatus(ctx, s.mon)
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
		if err := unfence(ctx, s.srcCfg); err != nil {
			s.rep.Event("error", "could not make the source writable again: "+message(err))
		} else {
			s.st.Fenced = false
		}
	}
	s.rep.SetDesired("run")
	s.rep.SetPhase(migrate.PhaseStreaming)
	s.rep.Event("error", "cutover cancelled: "+why+"; streaming continues")
}
