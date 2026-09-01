package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecResultRowCount(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.ExecResult(ctx, `INSERT INTO kv_state (k, v) VALUES ('offset', '41')`)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Errorf("insert RowsAffected = %d, %v; want 1, nil", n, err)
	}

	// The Telegram inbox's claim-then-send relies on 0 — not 1 — when the
	// guarded UPDATE matches nothing: the rowcount IS the claim verdict.
	res, err = s.ExecResult(ctx, `UPDATE kv_state SET v='42' WHERE k='missing'`)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 0 {
		t.Errorf("no-match update RowsAffected = %d, %v; want 0, nil", n, err)
	}
}

func TestExecBadStatement(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.Exec(ctx, `NOT SQL`); err == nil {
		t.Fatal("Exec of a malformed statement should return the batcher's error")
	}

	// A failed batch must not poison the batcher: the next write applies.
	mustExec(t, s, `INSERT INTO kv_state (k, v) VALUES ('after', 'ok')`)
	var v string
	if err := s.QueryRow(ctx, `SELECT v FROM kv_state WHERE k='after'`).Scan(&v); err != nil || v != "ok" {
		t.Errorf("post-failure write = %q, %v; want ok, nil", v, err)
	}
}

func TestCloseIdempotentAndRejectsWrites(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}

	// Write-shaped calls after Close fail fast with "store closed" — they
	// never panic or silently queue behind a dead batcher.
	if err := s.Flush(context.Background()); err == nil || err.Error() != "store closed" {
		t.Errorf("Flush after Close = %v, want %q", err, "store closed")
	}
	if err := s.enqueue(`SELECT 1`, nil, nil); err == nil || err.Error() != "store closed" {
		t.Errorf("enqueue after Close = %v, want %q", err, "store closed")
	}
}

// TestEnqueueFailsFastWhenQueueFull pins the saturation contract: when the
// write queue is full the producer gets an immediate error instead of
// buffering without bound. Enqueuing outpaces the batcher's transactions by
// orders of magnitude, so a tight loop is guaranteed to hit the cap.
func TestEnqueueFailsFastWhenQueueFull(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	query := `INSERT INTO audit_events (ts, trace_id, interface, decision_source, outcome)
		VALUES ('2026-09-01T00:00:00Z','t','cli','regex_router','ok')`
	full := false
	for i := 0; i < 100*writeQueueCap && !full; i++ {
		if err := s.enqueue(query, nil, nil); err != nil {
			if !strings.HasPrefix(err.Error(), "write queue full") {
				t.Fatalf("enqueue error = %v, want queue-full", err)
			}
			full = true
		}
	}
	if !full {
		t.Fatal("queue never filled; fail-fast branch not exercised")
	}
}

// TestCloseDrainsFailingWriteReportsError covers the drain error path: a
// malformed write still queued at Close must have its failure delivered to
// its done channel, not dropped silently on shutdown.
func TestCloseDrainsFailingWriteReportsError(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Enough bad writes that the batcher's main loop cannot consume them
	// all before quit fires, forcing some through the drain path.
	const bad = 200
	dones := make([]chan error, bad)
	for i := range dones {
		dones[i] = make(chan error, 1)
		if err := s.enqueue(`NOT SQL`, nil, dones[i]); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, done := range dones {
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("write %d drained with nil error, want the syntax failure", i)
			}
		default:
			t.Errorf("write %d: drain dropped its error report", i)
		}
	}
}

// TestCloseDrainsQueuedWrites pins the crash-safety guarantee for graceful
// shutdown: audit rows sitting in the write queue (no Flush called) must be
// persisted by Close alone, via the batcher's drain path. The backlog is
// large enough that the batcher cannot drain it through its main loop
// before quit fires, so the shutdown drain does real work.
func TestCloseDrainsQueuedWrites(t *testing.T) {
	ctx := context.Background()
	// A FILE-backed DB is required: an :memory: database dies with its
	// connections, so persistence across Close/reopen is only observable
	// on disk.
	path := filepath.Join(t.TempDir(), "drain.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const queued = 3000
	for i := 0; i < queued; i++ {
		s.Audit(ctx, AuditEvent{
			TraceID:        fmt.Sprintf("drain-%d", i),
			Interface:      IFaceCLI,
			DecisionSource: SourceRouter,
			RuleID:         "uptime",
			Prompt:         "/uptime",
		})
	}
	// White-box precondition: the backlog must still be queued when Close
	// fires, or the drain path does no work and the assertions below prove
	// nothing about shutdown.
	if len(s.writes) == 0 {
		t.Fatal("backlog drained before Close; drain path not exercised")
	}
	// Deliberately no Flush before Close.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	var n int
	if err := s2.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != queued {
		t.Errorf("persisted audit rows = %d, want %d (drain lost writes)", n, queued)
	}
	var traceID string
	if err := s2.QueryRow(ctx, `SELECT trace_id FROM audit_events WHERE trace_id=?`, "drain-99").Scan(&traceID); err != nil || traceID != "drain-99" {
		t.Errorf("last queued row not persisted: %q, %v", traceID, err)
	}
}

// TestReadsAfterCloseFailCleanly pins the shutdown-ordering contract: every
// read API must return an error — never panic — once the store is closed, so
// in-flight callers on the way down get a clean failure.
func TestReadsAfterCloseFailCleanly(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	reads := []struct {
		name string
		call func() error
	}{
		{"Query", func() error { _, err := s.Query(ctx, `SELECT 1`); return err }},
		{"QueryRow", func() error { return s.QueryRow(ctx, `SELECT 1`).Scan(new(int)) }},
		{"Stats", func() error { _, err := s.Stats(ctx); return err }},
		{"Replay", func() error { _, err := s.Replay(ctx, "tr"); return err }},
		{"ShadowStreak", func() error { _, err := s.ShadowStreak(ctx, "r"); return err }},
		{"RuleStates", func() error { _, err := s.RuleStates(ctx); return err }},
		{"NextMinedName", func() error { _, err := s.NextMinedName(ctx); return err }},
		{"GetAnswer", func() error { _, _, err := s.GetAnswer(ctx, "tr"); return err }},
	}
	for _, r := range reads {
		if err := r.call(); err == nil {
			t.Errorf("%s after Close = nil error, want a clean failure", r.name)
		}
	}
}

// TestOpenRejectsTamperedSchema pins the migration guardrail: a schema that
// no longer matches its recorded user_version (hand-rewound, or with a view
// squatting on a table name) must make Open fail loudly instead of serving a
// database it never verified.
func TestOpenRejectsTamperedSchema(t *testing.T) {
	cases := []struct {
		name    string
		tamper  []string
		wantErr string
	}{
		{
			name:    "user_version rewound to 2",
			tamper:  []string{`PRAGMA user_version=2`},
			wantErr: "migrating to v3", // ALTER hits the existing column
		},
		{
			name: "view squats on telegram_inbox",
			tamper: []string{
				`DROP TABLE telegram_inbox`,
				`CREATE VIEW telegram_inbox AS SELECT 1 AS update_id`,
				`PRAGMA user_version=1`,
			},
			wantErr: "migrating to v2", // CREATE INDEX on a view fails
		},
		{
			name: "view squats on audit_events",
			tamper: []string{
				`DROP TABLE audit_events`,
				`CREATE VIEW audit_events AS SELECT 1 AS id`,
				`PRAGMA user_version=0`,
			},
			wantErr: "migrating to v1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tampered.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range tc.tamper {
				mustExec(t, s, stmt)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			_, err = Open(path)
			if err == nil {
				t.Fatalf("Open on tampered schema (%s) should fail", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want a %q failure", err, tc.wantErr)
			}
		})
	}
}

func TestQueryRow(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	mustExec(t, s, `INSERT INTO kv_state (k, v) VALUES ('k', 'v1')`)

	var v string
	if err := s.QueryRow(ctx, `SELECT v FROM kv_state WHERE k='k'`).Scan(&v); err != nil || v != "v1" {
		t.Errorf("QueryRow hit = %q, %v; want v1, nil", v, err)
	}
	err := s.QueryRow(ctx, `SELECT v FROM kv_state WHERE k='nope'`).Scan(&v)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("QueryRow miss = %v, want sql.ErrNoRows", err)
	}
}

// Residual uncovered arms, deliberately not forced: execSync's ctx-race
// select arm, apply's BeginTx failure, the migrate Begin/Scan failures, and
// the mid-function Query/Scan error arms in stats/shadow/answers all require
// the driver or connection pool to fail between two statements of one call
// (or NOT NULL columns to hold unscannable data) — no public-API input
// reaches them, and a fault-injection harness is not worth the coupling.

func TestFlush(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	s.Audit(ctx, AuditEvent{
		TraceID: "tr", Interface: IFaceCLI, DecisionSource: SourceRouter, Prompt: "/uptime",
	})
	// The queued audit only becomes observable once the barrier write has
	// ridden the same queue and returned.
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var n int
	if err := s.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit rows after Flush = %d, want 1", n)
	}
}
