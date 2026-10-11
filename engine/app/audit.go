package app

import (
	"context"
	"encoding/json"
	"log"

	"dedie/engine/store"
)

// audit records an action in the organization's tamper-evident log. detail is
// marshalled to JSON and must never contain secrets.
func (a *App) audit(ctx context.Context, actor Actor, action, target string, detail any) {
	text := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			text = string(b)
		}
	}
	err := a.Store.Audit(context.WithoutCancel(ctx), actor.User.OrgID, actor.User.Email, action, target, text, actor.IP)
	if err != nil {
		log.Printf("audit write failed for %s by %s: %v", action, actor.User.Email, err)
	}
}

func (a *App) ListAudit(ctx context.Context, actor Actor, before int64, limit int) ([]store.AuditEntry, error) {
	if err := actor.require(PermViewAudit); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return a.Store.ListAudit(ctx, actor.User.OrgID, before, limit)
}

type AuditVerification struct {
	Intact   bool  `json:"intact"`
	Checked  int   `json:"checked"`
	BrokenAt int64 `json:"broken_at,omitempty"`
}

// VerifyAudit recomputes the hash chain and reports the first entry that was
// altered or removed, if any.
func (a *App) VerifyAudit(ctx context.Context, actor Actor) (AuditVerification, error) {
	if err := actor.require(PermViewAudit); err != nil {
		return AuditVerification{}, err
	}
	checked, broken, err := a.Store.VerifyAudit(ctx, actor.User.OrgID)
	return AuditVerification{Intact: broken == 0, Checked: checked, BrokenAt: broken}, err
}
