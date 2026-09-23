package telegram

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestParseEvery: valid forms, minute floor, and command extraction.
func TestParseEvery(t *testing.T) {
	d, cmd, err := parseEvery("/every 30m /disk")
	if err != nil || d != 30*time.Minute || cmd != "/disk" {
		t.Fatalf("got %v %q %v", d, cmd, err)
	}
	if _, _, err := parseEvery("/every 45s /disk"); err == nil {
		t.Fatal("45s must be rejected (minute floor)")
	}
	if _, _, err := parseEvery("/disk"); err == nil {
		t.Fatal("non-schedule text must be rejected")
	}
	d, cmd, err = parseEvery("every 2h /uptime extra args")
	if err != nil || d != 2*time.Hour || cmd != "/uptime extra args" {
		t.Fatalf("multiword: %v %q %v", d, cmd, err)
	}
}

// TestParseUnschedule: "#3" and "3" both accepted; junk rejected.
func TestParseUnschedule(t *testing.T) {
	for _, in := range []string{"/unschedule #3", "/unschedule 3"} {
		if id, err := parseUnschedule(in); err != nil || id != 3 {
			t.Fatalf("%q -> %d %v", in, id, err)
		}
	}
	if _, err := parseUnschedule("/unschedule abc"); err == nil {
		t.Fatal("non-numeric id must be rejected")
	}
}

// TestSchedulerRegisterListUnregister: full lifecycle without waiting on
// timers — Register returns an id, List shows it, Unregister stops it.
func TestSchedulerRegisterListUnregister(t *testing.T) {
	c := &fakeClient{}
	s := NewScheduler(fakeEngine{answer: "ok"}, c, []int64{1},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	out := s.Register(1, "/every 30m /disk")
	if !strings.Contains(out, "#1") {
		t.Fatalf("register reply = %q", out)
	}
	lst := s.List()
	if !strings.Contains(lst, "/disk") || !strings.Contains(lst, "#1") {
		t.Fatalf("list = %q", lst)
	}
	out = s.Unregister("/unschedule #1")
	if !strings.Contains(out, "unscheduled #1") {
		t.Fatalf("unregister reply = %q", out)
	}
	if lst = s.List(); lst != "no scheduled jobs" {
		t.Fatalf("post-unregister list = %q", lst)
	}
}

// TestSchedulerFiresAndSends: a 1-minute job with a compressed interval
// runs the engine and sends the reply to the registering chat.
func TestSchedulerFiresAndSends(t *testing.T) {
	c := &fakeClient{}
	s := NewScheduler(fakeEngine{answer: "disk fine"}, c, []int64{42},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Register(42, "/every 1m /disk")
	s.mu.Lock()
	for _, j := range s.jobs {
		j.nextRun = time.Now().Add(-time.Second) // force due now
	}
	s.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.sends) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(c.sends) == 0 {
		t.Fatal("scheduler never sent")
	}
	if !strings.Contains(c.sends[0], "⏰") || !strings.Contains(c.sends[0], "disk fine") {
		t.Fatalf("send = %q", c.sends[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	_ = cancel
	s.Unregister("/unschedule #1")
}

// TestSchedulerRejectsForeignChat: a non-allowlisted chat cannot schedule.
func TestSchedulerRejectsForeignChat(t *testing.T) {
	s := NewScheduler(fakeEngine{}, &fakeClient{}, []int64{1},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if out := s.Register(99, "/every 5m /uptime"); !strings.Contains(out, "allowlisted") {
		t.Fatalf("foreign chat reply = %q", out)
	}
}

// TestRegisterGatedAndOnDecided: build-time wiring hooks set the dispatcher
// fields — the gated path and decision hook become live.
func TestRegisterGatedAndOnDecided(t *testing.T) {
	d, _, _ := dispatcherWithStore(t, fakeEngine{answer: "x"}, &fakeClient{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	called := false
	d.RegisterGated(map[string]GatedAction{})
	d.OnDecided(func(id int64, v, by string) { called = true })
	if d.gated == nil {
		t.Fatal("gated map not wired")
	}
	if d.onDecided == nil {
		t.Fatal("onDecided not wired")
	}
	d.onDecided(1, "approved", "t")
	if !called {
		t.Fatal("hook not invoked")
	}
}

// TestEditorEnqueueAndDrain: NewEditor + EnqueueDecided + Run drain the
// queue and edit the recorded push (ADR-0009 happy path).
func TestEditorEnqueueAndDrain(t *testing.T) {
	c := &fakeClient{}
	reg := NewEditRegistry()
	reg.Record(7, 1, 100) // approval 7 lives in chat 1, message 100
	e := NewEditor(c, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.EnqueueDecided(7, "approved", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.edits)
		c.mu.Unlock()
		if n > 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("editor never drained the decision edit")
}

// TestStatsAliasMatchesStatus: /stats must render the same payload as
// /status (alias, not a second code path).
func TestStatsAliasMatchesStatus(t *testing.T) {
	d, _, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, &fakeClient{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetStats(st)
	got := d.statusText(context.Background())
	if !strings.Contains(got, "runs") || !strings.Contains(got, "AegisGo status") {
		t.Fatalf("statusText = %q", got)
	}
	// the switch case lists both names; assert the alias is wired by source
	src, err := os.ReadFile("dispatcher.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `case "/status", "/stats":`) {
		t.Fatal("/stats alias missing from dispatcher switch")
	}
}
