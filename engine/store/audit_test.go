package store

import (
	"context"
	"testing"
)

func TestAuditChainDetectsTampering(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 5; i++ {
		if err := s.Audit(ctx, "org1", "ada", "query.execute", "con_1", `{"sql":"select 1"}`, "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	if n, broken, err := s.VerifyAudit(ctx, "org1"); err != nil || n != 5 || broken != 0 {
		t.Fatalf("clean chain: checked=%d broken=%d err=%v", n, broken, err)
	}

	// Rewriting an entry breaks the chain at that entry.
	if _, err := s.db.Exec(`UPDATE audit SET detail='{"sql":"select 2"}' WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := s.VerifyAudit(ctx, "org1"); broken != 3 {
		t.Fatalf("edited entry: chain reported broken at %d, want 3", broken)
	}
	s.db.Exec(`UPDATE audit SET detail='{"sql":"select 1"}' WHERE id=3`)

	// Deleting an entry breaks it at the one that followed.
	if _, err := s.db.Exec(`DELETE FROM audit WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := s.VerifyAudit(ctx, "org1"); broken != 3 {
		t.Fatalf("deleted entry: chain reported broken at %d, want 3", broken)
	}
}
