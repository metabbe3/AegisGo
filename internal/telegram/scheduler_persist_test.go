package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/store"
)

var errBroken = errors.New("storage broken")

// TestSchedulePersistRoundTrip: with a store wired, Register persists the
// row; a fresh scheduler over the SAME DB rehydrates it at Start. This is
// the regression test for "nightly deploy wipes schedules" — it fails
// without the v8 persistence (Start would list nothing).
func TestSchedulePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/sched.db"
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	s1 := NewScheduler(fakeEngine{answer: "ok"}, &fakeClient{}, []int64{7},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s1.SetSchedules(st1)
	out := s1.Register(context.Background(), 7, "/every 30m /uptime")
	if !strings.Contains(out, "#1") {
		t.Fatalf("register reply = %q", out)
	}
	// stop timers of the live scheduler without touching the DB
	s1.mu.Lock()
	for _, j := range s1.jobs {
		close(j.stop)
	}
	s1.mu.Unlock()
	st1.Close() // "restart": process dies, DB survives

	// new process over the same DB: Start must rehydrate job #1
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	c := &fakeClient{}
	s2 := NewScheduler(fakeEngine{answer: "ok"}, c, []int64{7},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s2.SetSchedules(st2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s2.Start(ctx)

	lst := s2.List()
	if !strings.Contains(lst, "#1") || !strings.Contains(lst, "/uptime") {
		t.Fatalf("rehydrated list = %q (schedule lost on restart)", lst)
	}
	// the rehydrated id matches the DB row: unschedule deletes it durably
	if out := s2.Unregister(context.Background(), "/unschedule #1"); !strings.Contains(out, "unscheduled #1") {
		t.Fatalf("unregister = %q", out)
	}
	rows, err := st2.ListSchedules(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("post-unregister rows = %v err=%v (row must be deleted)", rows, err)
	}
	// and a restart after that rehydrates nothing
	st3, _ := store.Open(dbPath)
	defer st3.Close()
	s3 := NewScheduler(fakeEngine{}, &fakeClient{}, []int64{7}, nil)
	s3.SetSchedules(st3)
	s3.Start(context.Background())
	if lst := s3.List(); lst != "no scheduled jobs" {
		t.Fatalf("post-delete restart list = %q", lst)
	}
}

// TestScheduleRehydrateSkipsForeignAndTiny: rows whose chat is no longer
// allowlisted, or whose interval sits below the minute floor (manual DB
// edit), are skipped at rehydrate — the allowlist stays the boundary.
func TestScheduleRehydrateSkipsForeignAndTiny(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// chat 99 not allowlisted; interval 5s below floor
	if _, err := st.SaveSchedule(ctx, 99, "/uptime", (30 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveSchedule(ctx, 7, "/uptime", (5 * time.Second).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(fakeEngine{}, &fakeClient{}, []int64{7}, nil)
	s.SetSchedules(st)
	s.Start(ctx)
	if lst := s.List(); lst != "no scheduled jobs" {
		t.Fatalf("foreign/tiny rows must not launch, list = %q", lst)
	}
	// rows remain in the DB (audit trail), just not running
	rows, err := st.ListSchedules(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v err=%v (rows stay for audit)", rows, err)
	}
}

// TestScheduleRegisterPersistFailure: a broken persister answers honestly
// and registers nothing.
func TestScheduleRegisterPersistFailure(t *testing.T) {
	s := NewScheduler(fakeEngine{}, &fakeClient{}, []int64{1}, nil)
	s.SetSchedules(brokenPersister{})
	out := s.Register(context.Background(), 1, "/every 30m /uptime")
	if !strings.Contains(out, "could not save") {
		t.Fatalf("persist-failure reply = %q", out)
	}
	if lst := s.List(); lst != "no scheduled jobs" {
		t.Fatalf("nothing must be registered, list = %q", lst)
	}
}

type brokenPersister struct{}

func (brokenPersister) SaveSchedule(context.Context, int64, string, int64) (int64, error) {
	return 0, errBroken
}
func (brokenPersister) DeleteSchedule(context.Context, int64) (bool, error) { return false, errBroken }
func (brokenPersister) ListSchedules(context.Context) ([]store.SchedJob, error) {
	return nil, errBroken
}
