package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type AuditEntry struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail"`
	IP     string    `json:"ip"`
	Hash   string    `json:"hash"`
}

func auditHash(prev, orgID, ts, actor, action, target, detail, ip string) string {
	h := sha256.New()
	for _, p := range []string{prev, orgID, ts, actor, action, target, detail, ip} {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Audit appends an entry to the org's hash chain: each entry's hash covers its
// content and the previous entry's hash, so edits and deletions are detectable.
func (s *Store) Audit(ctx context.Context, orgID, actor, action, target, detail, ip string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	err = tx.QueryRowContext(ctx, `SELECT hash FROM audit WHERE org_id=? ORDER BY id DESC LIMIT 1`, orgID).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ts := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(org_id,ts,actor,action,target,detail,ip,prev_hash,hash) VALUES(?,?,?,?,?,?,?,?,?)`,
		orgID, ts, actor, action, target, detail, ip, prev, auditHash(prev, orgID, ts, actor, action, target, detail, ip)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListAudit(ctx context.Context, orgID string, beforeID int64, limit int) ([]AuditEntry, error) {
	if beforeID <= 0 {
		beforeID = 1 << 62
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,ts,actor,action,target,detail,ip,hash FROM audit WHERE org_id=? AND id<? ORDER BY id DESC LIMIT ?`, orgID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.IP, &e.Hash); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyAudit recomputes the org's hash chain. It returns the number of
// entries checked and the id of the first broken entry (0 when intact).
func (s *Store) VerifyAudit(ctx context.Context, orgID string) (checked int, brokenAt int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,ts,actor,action,target,detail,ip,prev_hash,hash FROM audit WHERE org_id=? ORDER BY id`, orgID)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	prev := ""
	for rows.Next() {
		var id int64
		var ts, actor, action, target, detail, ip, prevHash, hash string
		if err := rows.Scan(&id, &ts, &actor, &action, &target, &detail, &ip, &prevHash, &hash); err != nil {
			return checked, 0, err
		}
		if prevHash != prev || hash != auditHash(prev, orgID, ts, actor, action, target, detail, ip) {
			return checked, id, nil
		}
		prev = hash
		checked++
	}
	return checked, 0, rows.Err()
}
