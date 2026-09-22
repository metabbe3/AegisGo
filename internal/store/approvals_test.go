package store

import (
	"context"
	"testing"
	"time"
)

// newTestStore opens an in-memory store (same helper style as store_test.go).
func newApprovalTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestApprovalLifecycleApprove(t *testing.T) {
	s := newApprovalTestStore(t)
	ctx := context.Background()

	id, err := s.CreateApproval(ctx, "system_command", `{"command":"restart"}`, "L2: service restart")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want > 0", id)
	}

	got, ok, err := s.GetApproval(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.State != "pending" || got.Kind != "system_command" || got.DecidedBy != "" {
		t.Errorf("initial row wrong: %+v", got)
	}

	ok, err = s.DecideApproval(ctx, id, "approved", "telegram:500212724")
	if err != nil || !ok {
		t.Fatalf("decide approve: ok=%v err=%v", ok, err)
	}

	got, _, _ = s.GetApproval(ctx, id)
	if got.State != "approved" || got.DecidedBy != "telegram:500212724" || got.DecidedAt == "" {
		t.Errorf("approved row wrong: %+v", got)
	}

	// Idempotence: deciding again must report false, not error or flip state.
	ok, err = s.DecideApproval(ctx, id, "denied", "cli")
	if err != nil {
		t.Fatalf("second decide errored: %v", err)
	}
	if ok {
		t.Error("second decide should be a no-op (false), got true")
	}
	got, _, _ = s.GetApproval(ctx, id)
	if got.State != "approved" {
		t.Errorf("state flipped on double-decide: %q", got.State)
	}
}

func TestApprovalDeny(t *testing.T) {
	s := newApprovalTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateApproval(ctx, "sql_query", `{"q":"DELETE"}`, "L2: write sql")
	ok, err := s.DecideApproval(ctx, id, "denied", "cli")
	if err != nil || !ok {
		t.Fatalf("deny: ok=%v err=%v", ok, err)
	}
	got, _, _ := s.GetApproval(ctx, id)
	if got.State != "denied" {
		t.Errorf("state = %q, want denied", got.State)
	}
}

func TestApprovalInvalidStateRejected(t *testing.T) {
	s := newApprovalTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateApproval(ctx, "k", "{}", "r")
	if _, err := s.DecideApproval(ctx, id, "maybe", "cli"); err == nil {
		t.Error("state 'maybe' must be rejected")
	}
}

func TestApprovalExpirySweepsOnlyOldPending(t *testing.T) {
	s := newApprovalTestStore(t)
	ctx := context.Background()
	old, _ := s.CreateApproval(ctx, "k1", "{}", "old")
	fresh, _ := s.CreateApproval(ctx, "k2", "{}", "fresh")

	// Age the first row beyond ttl by writing created_at directly (test-only).
	if err := s.Exec(ctx, `UPDATE approvals SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339Nano), old); err != nil {
		t.Fatalf("age row: %v", err)
	}

	n, err := s.ExpireApprovals(ctx, time.Hour)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expired %d rows, want 1", n)
	}
	a, _, _ := s.GetApproval(ctx, old)
	if a.State != "expired" {
		t.Errorf("old row state = %q, want expired", a.State)
	}
	b, _, _ := s.GetApproval(ctx, fresh)
	if b.State != "pending" {
		t.Errorf("fresh row state = %q, want pending", b.State)
	}

	// Expired rows can no longer be decided (fail-closed).
	ok, err := s.DecideApproval(ctx, old, "approved", "cli")
	if err != nil {
		t.Fatalf("decide expired: %v", err)
	}
	if ok {
		t.Error("deciding an expired approval must be a no-op")
	}
}

func TestPendingApprovalsListsOldestFirstCapped(t *testing.T) {
	s := newApprovalTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.CreateApproval(ctx, "k", "{}", "r")
	}
	s.DecideApproval(ctx, 2, "approved", "cli") // hole in the middle

	pend, err := s.PendingApprovals(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pend) != 2 || pend[0].ID != 1 || pend[1].ID != 3 {
		t.Errorf("pending = %+v, want ids [1 3]", pend)
	}
}

func TestGetApprovalMissing(t *testing.T) {
	s := newApprovalTestStore(t)
	if _, ok, err := s.GetApproval(context.Background(), 9999); err != nil || ok {
		t.Errorf("missing row: ok=%v err=%v", ok, err)
	}
}

// RecentDecisions: newest-first, pending excluded, capped.
func TestRecentDecisions(t *testing.T) {
	st := newApprovalTestStore(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		id, err := st.CreateApproval(ctx, "k", `{}`, "r")
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if _, err := st.DecideApproval(ctx, id, "approved", "telegram:1"); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := st.RecentDecisions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 decided, got %d", len(got))
	}
	if got[0].ID < got[1].ID {
		t.Fatalf("not newest-first: %v then %v", got[0].ID, got[1].ID)
	}
	if got[0].State != "approved" || got[0].DecidedBy != "telegram:1" {
		t.Fatalf("row wrong: %+v", got[0])
	}
}
