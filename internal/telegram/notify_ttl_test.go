package telegram

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeApprovalSource struct {
	pend []ApprovalInfo
}

func (f *fakeApprovalSource) PendingApprovals(context.Context, int) ([]ApprovalInfo, error) {
	return f.pend, nil
}

// TestApprovalReminderAfterTTL: an approval still pending past the TTL is
// re-announced exactly once per reminder window; a decided one is not.
func TestApprovalReminderAfterTTL(t *testing.T) {
	src := &fakeApprovalSource{pend: []ApprovalInfo{{
		ID: 7, Kind: "system_command", Reason: "ls -la",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	}}}
	c := &fakeClient{}
	n := NewNotifier(src, c, []int64{1}, 5*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Compress the window: due immediately.
	n.reminderAfter = time.Minute

	n.tick(context.Background()) // first announce (new id)
	if got := len(c.sends); got != 1 {
		t.Fatalf("first tick sends = %d, want 1", got)
	}
	n.tick(context.Background()) // still within first reminder window
	if got := len(c.sends); got != 2 {
		t.Fatalf("reminder tick sends = %d, want 2 (announce+reminder)", got)
	}
	if !strings.Contains(c.sends[len(c.sends)-1], "Approval needed") {
		t.Fatalf("reminder body = %q", c.sends[len(c.sends)-1])
	}
}

// TestApprovalNoReminderWithoutCreatedAt: sources without CreatedAt never
// remind (unknown age must not fabricate a nudge).
func TestApprovalNoReminderWithoutCreatedAt(t *testing.T) {
	src := &fakeApprovalSource{pend: []ApprovalInfo{{ID: 9, Kind: "k", Reason: "r"}}}
	c := &fakeClient{}
	n := NewNotifier(src, c, []int64{1}, 5*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.tick(context.Background())
	n.tick(context.Background())
	n.tick(context.Background())
	if got := len(c.sends); got != 1 {
		t.Fatalf("sends = %d, want 1 (announce only, no reminders)", got)
	}
}
