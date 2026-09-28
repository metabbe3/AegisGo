package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"aegisgo/internal/store"
)

// healthHarness: a real dispatcher over a real :memory: store so the
// /health path is exercised against the same SQL the live bot runs.
func healthHarness(t *testing.T) (*Dispatcher, *Inbox, *fakeClient, *store.Store) {
	t.Helper()
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetHealth(st)
	return d, inbox, c, st
}

// seedAuditRows inserts outcome rows the way the engine's finisher does.
func seedAuditRows(t *testing.T, st *store.Store, rows []auditRow) {
	t.Helper()
	for i, r := range rows {
		st.Audit(context.Background(), auditEventOf(r, i))
	}
}

type auditRow struct {
	src  string
	out  string
	lat  int64
	conf int
}

func auditEventOf(r auditRow, i int) store.AuditEvent {
	return store.AuditEvent{
		TraceID:        fmt.Sprintf("trace-health-%d", i),
		Interface:      "telegram",
		DecisionSource: r.src,
		LatencyMS:      r.lat,
		Outcome:        r.out,
		Confidence:     r.conf,
	}
}

// TestHealthCommand: /health renders per-source outcome quality — error
// counts and confidence must surface, not just run counts.
func TestHealthCommand(t *testing.T) {
	d, inbox, c, st := healthHarness(t)
	seedAuditRows(t, st, []auditRow{
		{"regex_router", "ok", 6, 100},
		{"regex_router", "ok", 10, 100},
		{"regex_router", "error", 8, 0},
		{"llm", "ok", 11500, 55},
	})
	d.Process(t.Context(), feed(t, inbox, 9201, "/health"))

	got := lastSend(c)
	for _, want := range []string{"🧪", "runs 4", "errors 1 (25.0%)", "Router 3 runs", "1 err", "conf 67", "LLM 1 runs", "conf 55"} {
		if !strings.Contains(got, want) {
			t.Fatalf("health text missing %q: %q", want, got)
		}
	}
}

// TestHealthEmpty: a fresh store answers honestly, not with a blank line.
func TestHealthEmpty(t *testing.T) {
	d, inbox, c, _ := healthHarness(t)
	d.Process(t.Context(), feed(t, inbox, 9202, "/health"))

	if got := lastSend(c); !strings.Contains(got, "no runs recorded yet") {
		t.Fatalf("empty health text = %q", got)
	}
}

// TestHealthUnwired: no source wired degrades honestly — and the
// unknown-command guard must NOT claim /health (it is a known command).
func TestHealthUnwired(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.Process(t.Context(), feed(t, inbox, 9203, "/health"))

	got := lastSend(c)
	if !strings.Contains(got, "unavailable") {
		t.Fatalf("unwired health text = %q", got)
	}
	if strings.Contains(got, "Unknown command") {
		t.Fatalf("known command claimed unknown: %q", got)
	}
}

// TestHealthListedInHelp: discovery works — the command appears in /help.
func TestHealthListedInHelp(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.Process(t.Context(), feed(t, inbox, 9204, "/help"))

	if got := lastSend(c); !strings.Contains(got, "/health") {
		t.Fatalf("help missing /health: %q", got)
	}
}
