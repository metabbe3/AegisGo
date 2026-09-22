package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// notifierStore wraps a real :memory: store (the ledger is the contract).
func newNotifierStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestNotifierAnnouncesOnlyNewApprovals(t *testing.T) {
	st := newNotifierStore(t)
	ctx := context.Background()
	// Pre-existing pending row — must be primed as "seen", NOT announced.
	st.CreateApproval(ctx, "old_kind", "{}", "pre-boot pending")

	c := &fakeClient{}
	src := &approvalShim{st: st}
	n := NewNotifier(src, c, []int64{chatOK}, 10*time.Millisecond, nil)

	n.prime(ctx)
	if n.lastSeen != 1 {
		t.Fatalf("prime lastSeen = %d, want 1", n.lastSeen)
	}

	// New approval arrives AFTER boot → must be announced exactly once.
	st.CreateApproval(ctx, "system_command", `{"command":"restart"}`, "L2 restart")
	n.tick(ctx)
	sends := c.sendCount()
	if sends == 0 {
		t.Fatal("new approval was not announced")
	}
	last := lastSend(c)
	if !contains(last, "#2 · System command") {
		t.Errorf("announce text wrong: %q", last)
	}
	if len(c.lastButtons) != 1 || len(c.lastButtons[0]) != 2 ||
		c.lastButtons[0][0].Data != "apr:2" || c.lastButtons[0][1].Data != "dny:2" {
		t.Errorf("announce buttons wrong: %+v", c.lastButtons)
	}
	if contains(last, "/approve 2") {
		t.Errorf("text still carries the typed hint — buttons replace it: %q", last)
	}

	// Same pending row on the next tick → NOT re-announced.
	n.tick(ctx)
	if c.sendCount() != sends {
		t.Errorf("re-announced same approval: %d sends", c.sendCount())
	}
}

func TestNotifierNoChatsDisabled(t *testing.T) {
	st := newNotifierStore(t)
	ctx := context.Background()
	c := &fakeClient{}
	n := NewNotifier(&approvalShim{st: st}, c, nil, time.Millisecond, nil)
	n.Start(ctx) // returns immediately
	st.CreateApproval(ctx, "k", "{}", "r")
	time.Sleep(30 * time.Millisecond)
	if c.sendCount() != 0 {
		t.Errorf("disabled notifier sent %d messages", c.sendCount())
	}
}

func TestNotifierEndToEndGoroutine(t *testing.T) {
	st := newNotifierStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &fakeClient{}
	n := NewNotifier(&approvalShim{st: st}, c, []int64{chatOK}, 10*time.Millisecond, nil)
	go n.Start(ctx)

	time.Sleep(30 * time.Millisecond) // prime lands
	st.CreateApproval(context.Background(), "k2", "{}", "created while running")
	deadline := time.Now().Add(2 * time.Second)
	for c.sendCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.sendCount() == 0 {
		t.Fatal("notifier never announced the live approval")
	}
	if !contains(lastSend(c), "#1 · K2") {
		t.Errorf("live announce wrong: %q", lastSend(c))
	}
}

// approvalShim mirrors app.go's adapter (kept local so the test owns it).
type approvalShim struct{ st *store.Store }

func (s *approvalShim) PendingApprovals(ctx context.Context, limit int) ([]ApprovalInfo, error) {
	rows, err := s.st.PendingApprovals(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalInfo, 0, len(rows))
	for _, a := range rows {
		out = append(out, ApprovalInfo{ID: a.ID, Kind: a.Kind, Reason: a.Reason, Payload: a.Payload})
	}
	return out, nil
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = sync.Mutex{} // keep sync if unused later
