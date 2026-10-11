package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dedie/engine/backup"
	"dedie/engine/connector"
	"dedie/engine/store"
)

// walProcs tracks the log receivers this process is running, by connection.
type walProcs struct {
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// WALStatus is the state of a connection's log archive.
type WALStatus struct {
	store.WALArchive
	ConnectionName string `json:"connection_name"`
	// Receiving reports whether the log is being received right now, by
	// this process.
	Receiving bool  `json:"receiving"`
	Segments  int   `json:"segments"`
	SizeBytes int64 `json:"size_bytes"`
	// NewestAt is when the archive last received something.
	NewestAt time.Time `json:"newest_at"`
	// EarliestRecovery is the earliest moment the server can be recovered
	// to: the end of the oldest physical backup taken since archiving
	// began. Zero means no such backup exists yet.
	EarliestRecovery time.Time `json:"earliest_recovery"`
}

func walSlot(connID string) string {
	id := strings.ToLower(strings.TrimPrefix(connID, "con_"))
	if len(id) > 12 {
		id = id[:12]
	}
	return "dedie_wal_" + id
}

func (a *App) walStatus(ctx context.Context, w store.WALArchive, name string) WALStatus {
	st := WALStatus{WALArchive: w, ConnectionName: name}
	a.wal.mu.Lock()
	_, st.Receiving = a.wal.running[w.ConnectionID]
	a.wal.mu.Unlock()
	entries, _ := os.ReadDir(w.Dir)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		st.Segments++
		st.SizeBytes += info.Size()
		if info.ModTime().After(st.NewestAt) {
			st.NewestAt = info.ModTime()
		}
	}
	if w.Enabled {
		list, _ := a.Store.ConnectionBackups(ctx, w.OrgID, w.ConnectionID)
		for _, b := range list {
			if b.Type == backup.TypePhysical && b.CreatedAt.After(w.StartedAt) && (st.EarliestRecovery.IsZero() || b.FinishedAt.Before(st.EarliestRecovery)) {
				st.EarliestRecovery = b.FinishedAt
			}
		}
	}
	return st
}

// WALArchives reports the log archive of every PostgreSQL connection the
// actor may see, whether or not it is switched on.
func (a *App) WALArchives(ctx context.Context, actor Actor) ([]WALStatus, error) {
	if err := actor.require(PermBackup); err != nil {
		return nil, err
	}
	conns, err := a.visibleConnections(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := []WALStatus{}
	for _, c := range conns {
		if c.Kind != "postgres" {
			continue
		}
		w, err := a.Store.GetWALArchive(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			w = store.WALArchive{ConnectionID: c.ID, OrgID: c.OrgID, Dir: a.walDir(c.ID), Slot: walSlot(c.ID)}
		} else if err != nil {
			return nil, err
		}
		out = append(out, a.walStatus(ctx, w, c.Name))
	}
	return out, nil
}

func (a *App) walDir(connID string) string { return filepath.Join(a.dataDir, "wal", connID) }

// SetWALArchive switches continuous archiving of a PostgreSQL server's
// write-ahead log on or off. On, a running dedie-server receives the log
// as it is written. Off, the receiver stops and its replication slot is
// dropped so the server no longer holds log for it; the segments already
// received are kept.
func (a *App) SetWALArchive(ctx context.Context, actor Actor, connID string, enabled bool, retentionDays int) (WALStatus, error) {
	if err := actor.require(PermRestore); err != nil {
		return WALStatus{}, err
	}
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return WALStatus{}, err
	}
	if c.Kind != "postgres" {
		return WALStatus{}, invalid("point-in-time recovery is available for PostgreSQL")
	}
	if retentionDays < 0 {
		return WALStatus{}, invalid("retention cannot be negative")
	}
	w, err := a.Store.GetWALArchive(ctx, c.ID)
	if errors.Is(err, store.ErrNotFound) {
		w = store.WALArchive{ConnectionID: c.ID, OrgID: c.OrgID, Dir: a.walDir(c.ID), Slot: walSlot(c.ID)}
	} else if err != nil {
		return WALStatus{}, err
	}
	if enabled && !w.Enabled {
		// A new window starts now: log from before this moment was not kept.
		w.StartedAt = time.Now().UTC()
	}
	wasEnabled := w.Enabled
	w.Enabled, w.RetentionDays, w.UpdatedBy = enabled, retentionDays, actor.User.Email
	if err := a.Store.SaveWALArchive(ctx, w); err != nil {
		return WALStatus{}, err
	}
	if wasEnabled && !enabled {
		a.stopWAL(c.ID)
		// Without this the server would keep its log for a reader that
		// is never coming back, until its disk filled.
		_, sess, err := a.open(ctx, actor, c.ID)
		if err == nil {
			_, derr := sess.Query(ctx, fmt.Sprintf("SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = '%s'", w.Slot),
				connector.QueryOptions{MaxRows: 1})
			sess.Close(context.WithoutCancel(ctx))
			err = derr
		}
		if err != nil {
			a.Store.SetWALError(ctx, c.ID, "archiving is off, but the replication slot "+w.Slot+" could not be dropped: "+err.Error()+". Drop it on the server so it stops holding log")
		}
	}
	a.audit(ctx, actor, "backup.wal_archive", c.ID, map[string]any{"connection": c.Name, "enabled": enabled, "retention_days": retentionDays})
	w, _ = a.Store.GetWALArchive(ctx, c.ID)
	return a.walStatus(ctx, w, c.Name), nil
}

func (a *App) stopWAL(connID string) {
	a.wal.mu.Lock()
	if cancel, ok := a.wal.running[connID]; ok {
		cancel()
		delete(a.wal.running, connID)
	}
	a.wal.mu.Unlock()
}

// RunWALArchiver keeps a log receiver running for every connection with
// archiving switched on, restarts one that stops, and applies retention.
// It returns when ctx is cancelled.
func (a *App) RunWALArchiver(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		archives, err := a.Store.WALArchives(ctx, "")
		if err != nil && ctx.Err() == nil {
			log.Printf("wal archiver: %v", err)
		}
		for _, w := range archives {
			if !w.Enabled {
				a.stopWAL(w.ConnectionID)
				continue
			}
			a.wal.mu.Lock()
			_, running := a.wal.running[w.ConnectionID]
			a.wal.mu.Unlock()
			if !running {
				a.startWAL(ctx, w)
			}
		}
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			for _, w := range archives {
				if w.Enabled && w.RetentionDays > 0 {
					pruneWAL(w.Dir, time.Now().AddDate(0, 0, -w.RetentionDays))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// startWAL runs one receiver until it exits or is stopped.
func (a *App) startWAL(ctx context.Context, w store.WALArchive) {
	c, err := a.Store.GetConnection(ctx, w.OrgID, w.ConnectionID)
	if err != nil {
		return
	}
	cfg, err := a.config(c)
	if err != nil {
		a.Store.SetWALError(ctx, w.ConnectionID, err.Error())
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	var stderr bytes.Buffer
	cmd, cleanup, err := backup.ReceiveWAL(runCtx, cfg, w.Dir, w.Slot, &stderr)
	if err != nil {
		cancel()
		msg := err.Error()
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			msg += ": " + tail
		}
		a.Store.SetWALError(ctx, w.ConnectionID, msg)
		return
	}
	a.wal.mu.Lock()
	a.wal.running[w.ConnectionID] = cancel
	a.wal.mu.Unlock()
	a.Store.SetWALError(ctx, w.ConnectionID, "")
	go func() {
		err := cmd.Wait()
		cleanup()
		a.wal.mu.Lock()
		delete(a.wal.running, w.ConnectionID)
		a.wal.mu.Unlock()
		// A receiver that was stopped on purpose has nothing to report.
		if runCtx.Err() == nil && err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 500 {
				msg = msg[len(msg)-500:]
			}
			a.Store.SetWALError(context.Background(), w.ConnectionID, "the log receiver stopped and will be restarted: "+msg)
		}
		cancel()
	}()
}

// pruneWAL deletes completed segments last written before a time. The
// segment in progress is never touched.
func pruneWAL(dir string, before time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Recovery describes a data directory prepared for point-in-time recovery.
type Recovery struct {
	Backup store.Backup `json:"backup"`
	Dir    string       `json:"dir"`
	Target time.Time    `json:"target"`
	Files  int          `json:"files"`
}

// RecoverToTime builds, in an empty directory on this machine, a
// PostgreSQL data directory that recovers the connection's server to a
// moment in time when it is started: the newest physical backup taken
// before that moment, set up to replay the archived log up to it. A zero
// target recovers to the most recent change received.
func (a *App) RecoverToTime(ctx context.Context, actor Actor, connID string, target time.Time, dir string) (Recovery, error) {
	if err := actor.require(PermRestore); err != nil {
		return Recovery{}, err
	}
	c, err := a.conn(ctx, actor, connID)
	if err != nil {
		return Recovery{}, err
	}
	w, err := a.Store.GetWALArchive(ctx, c.ID)
	if err != nil {
		return Recovery{}, invalid("log archiving has never been switched on for %s, so there is no log to recover with", c.Name)
	}
	if !target.IsZero() && target.After(time.Now()) {
		return Recovery{}, invalid("the moment to recover to is in the future")
	}
	list, err := a.Store.ConnectionBackups(ctx, c.OrgID, c.ID)
	if err != nil {
		return Recovery{}, err
	}
	// Recovery starts from a backup that finished before the moment and
	// was taken while the log was being archived, so that every change
	// after it is on hand.
	sort.Slice(list, func(i, j int) bool { return list[i].FinishedAt.After(list[j].FinishedAt) })
	var base *store.Backup
	for i, b := range list {
		if b.Type != backup.TypePhysical || b.CreatedAt.Before(w.StartedAt) {
			continue
		}
		if target.IsZero() || b.FinishedAt.Before(target) {
			base = &list[i]
			break
		}
	}
	if base == nil {
		return Recovery{}, invalid("there is no physical backup of %s that finished before that moment and after log archiving began (%s). Recovery can only reach moments after such a backup",
			c.Name, w.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	files, err := a.ExtractBackup(ctx, actor, base.ID, dir)
	if err != nil {
		return Recovery{}, err
	}
	if err := backup.PrepareRecovery(dir, w.Dir, target); err != nil {
		return Recovery{}, err
	}
	a.audit(ctx, actor, "backup.recover", base.ID, map[string]any{"connection": c.Name, "directory": dir, "target": target})
	return Recovery{Backup: *base, Dir: dir, Target: target, Files: files}, nil
}
