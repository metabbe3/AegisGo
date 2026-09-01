package router

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// waitFor polls cond every ~10ms until it holds or the deadline passes, then
// fails the test. Hot-reload timing is observed, never assumed via sleeps.
func waitFor(t *testing.T, cond func() bool, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", deadline)
}

func openRulesStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ruleNames(r *Router) map[string]bool {
	names := make(map[string]bool)
	for _, d := range r.RuleDefs() {
		names[d.Name] = true
	}
	return names
}

func insertRule(ctx context.Context, t *testing.T, st *store.Store, name, pattern, tool, args string) {
	t.Helper()
	err := st.Exec(ctx,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts)
		 VALUES (?,?,?,?,?,1,?)`,
		name, pattern, tool, args, "seed", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
}

func countRules(ctx context.Context, t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.QueryRow(ctx, `SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoadRulesSeedsFreshDB(t *testing.T) {
	ctx := context.Background()
	st := openRulesStore(t)

	defs, err := LoadRules(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(Seeded()) {
		t.Fatalf("defs = %d, want %d (full seed set)", len(defs), len(Seeded()))
	}
	for _, d := range defs {
		if d.State != RuleActive {
			t.Errorf("rule %s state = %q, want active", d.Name, d.State)
		}
	}
	if n := countRules(ctx, t, st); n != len(Seeded()) {
		t.Errorf("rules table rows = %d, want %d (seeds persisted)", n, len(Seeded()))
	}
}

func TestLoadRulesReadsEnabledOnly(t *testing.T) {
	ctx := context.Background()
	st := openRulesStore(t)
	if _, err := LoadRules(ctx, st); err != nil {
		t.Fatal(err)
	}

	// Disable one seed; add one enabled custom rule.
	if err := st.Exec(ctx, `UPDATE rules SET enabled=0 WHERE name='uptime'`); err != nil {
		t.Fatal(err)
	}
	insertRule(ctx, t, st, "custom", "/custom", "system_command", `{"command":"hostname"}`)

	defs, err := LoadRules(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(Seeded()) {
		t.Fatalf("defs = %d, want %d (one disabled, one added)", len(defs), len(Seeded()))
	}
	sawCustom := false
	for _, d := range defs {
		if d.Name == "uptime" {
			t.Error("disabled rule returned by LoadRules")
		}
		if d.Name == "custom" {
			sawCustom = true
		}
	}
	if !sawCustom {
		t.Error("enabled custom rule missing from LoadRules result")
	}

	// A non-empty result must not re-seed: row count is unchanged by the
	// second load above.
	if n := countRules(ctx, t, st); n != len(Seeded())+1 {
		t.Errorf("rules table rows = %d, want %d (no reseed on non-empty result)", n, len(Seeded())+1)
	}
}

// TestLoadRulesAllDisabledReseeds pins LoadRules's fresh-boot fallback: when
// no enabled rows exist, the table is treated as empty and the in-code seed
// set is restored (active) as the result. This is intentional — a fully
// disabled rules table must not leave the router rule-less, so the
// deterministic defaults come back. The reseed itself is INSERT OR IGNORE
// against existing (disabled) names: rows are neither duplicated nor
// re-enabled; the recovery lives in the returned defs, and the next boot
// re-derives it the same way.
func TestLoadRulesAllDisabledReseeds(t *testing.T) {
	ctx := context.Background()
	st := openRulesStore(t)
	if _, err := LoadRules(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `UPDATE rules SET enabled=0`); err != nil {
		t.Fatal(err)
	}

	defs, err := LoadRules(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(Seeded()) {
		t.Fatalf("defs = %d, want %d (seed set restored)", len(defs), len(Seeded()))
	}
	sawUptime := false
	for _, d := range defs {
		if d.State != RuleActive {
			t.Errorf("rule %s state = %q, want active", d.Name, d.State)
		}
		if d.Name == "uptime" {
			sawUptime = true
		}
	}
	if !sawUptime {
		t.Error("restored set missing uptime")
	}

	if n := countRules(ctx, t, st); n != len(Seeded()) {
		t.Errorf("rules table rows = %d, want %d (INSERT OR IGNORE must not duplicate disabled names)", n, len(Seeded()))
	}
}

func TestStartHotReloadDisabled(t *testing.T) {
	st := openRulesStore(t)
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}
	// Interval 0 (with the nil-logger branch taken too) → immediate no-op stop.
	stop := StartHotReload(r, st, 0, nil)
	if stop == nil {
		t.Fatal("nil stop func returned")
	}
	stop()
	stop() // safe to call twice: the disabled path has nothing to close.

	if d := r.Handle(context.Background(), "/uptime"); !d.Handled || d.RuleID != "uptime" || d.Err != nil {
		t.Errorf("router disturbed by disabled hot reload: %+v", d)
	}
}

func TestStartHotReloadSwapsInNewRule(t *testing.T) {
	ctx := context.Background()
	st := openRulesStore(t)
	defs, err := LoadRules(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(testRegistry(t), defs)
	if err != nil {
		t.Fatal(err)
	}

	stop := StartHotReload(r, st, 50*time.Millisecond, nil)
	t.Cleanup(stop) // registered after st.Close's cleanup → runs before it

	insertRule(ctx, t, st, "ping", "/ping", "system_command", `{"command":"hostname"}`)

	waitFor(t, func() bool { return ruleNames(r)["ping"] }, 2*time.Second)

	d := r.Handle(ctx, "/ping")
	if !d.Handled || d.RuleID != "ping" || d.Err != nil {
		t.Fatalf("/ping after hot reload: %+v", d)
	}
}

// recHandler records message strings so a failed reload tick is directly
// observable — a rejected Swap leaves no trace in the live rule set.
type recHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

func (h *recHandler) saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func TestStartHotReloadBadRowKeepsPrevious(t *testing.T) {
	ctx := context.Background()
	st := openRulesStore(t)
	defs, err := LoadRules(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(testRegistry(t), defs)
	if err != nil {
		t.Fatal(err)
	}

	h := &recHandler{}
	stop := StartHotReload(r, st, 50*time.Millisecond, slog.New(h))
	t.Cleanup(stop)

	// Corrupt one rule's pattern. Every subsequent load returns a def that
	// cannot compile, so Swap must reject the whole set and keep the live
	// rules (log, don't crash).
	if err := st.Exec(ctx, `UPDATE rules SET pattern='(' WHERE name='uptime'`); err != nil {
		t.Fatal(err)
	}

	// Deterministic proof a tick read the bad row and survived: the reload
	// loop logs the rejection and keeps ticking.
	waitFor(t, func() bool { return h.saw("rules swap rejected; keeping previous rules") }, 2*time.Second)

	// Previous rules still serve — including the rule whose stored row is
	// now corrupt (its previously compiled form lives on).
	if d := r.Handle(ctx, "/uptime"); !d.Handled || d.RuleID != "uptime" || d.Err != nil {
		t.Errorf("previous rules lost after bad row: %+v", d)
	}
	if d := r.Handle(ctx, "/hostname"); !d.Handled || d.RuleID != "hostname" || d.Err != nil {
		t.Errorf("sibling rule lost after bad row: %+v", d)
	}

	// Repair the row and confirm full recovery through a valid, observable
	// reload — the loop processed the bad row and kept working.
	if err := st.Exec(ctx, `UPDATE rules SET pattern='/uptime' WHERE name='uptime'`); err != nil {
		t.Fatal(err)
	}
	insertRule(ctx, t, st, "ping", "/ping", "system_command", `{"command":"hostname"}`)
	waitFor(t, func() bool { return ruleNames(r)["ping"] }, 2*time.Second)
	if d := r.Handle(ctx, "/ping"); !d.Handled || d.RuleID != "ping" || d.Err != nil {
		t.Errorf("/ping after recovery: %+v", d)
	}
}

// brokenQueryer fails every database call (corrupt DB, closed handle).
type brokenQueryer struct{}

func (brokenQueryer) Query(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("rules table unreadable")
}
func (brokenQueryer) Exec(context.Context, string, ...any) error {
	return errors.New("rules table unwritable")
}

// failExec reads through a real store but fails every write, exercising the
// seeding path's error branch (read finds nothing, seed insert fails).
type failExec struct{ st *store.Store }

func (f failExec) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return f.st.Query(ctx, q, args...)
}
func (failExec) Exec(context.Context, string, ...any) error {
	return errors.New("rules table unwritable")
}

func TestLoadRulesAndReloadSurfaceStoreErrors(t *testing.T) {
	ctx := context.Background()

	// Read failure propagates to the caller.
	if _, err := LoadRules(ctx, brokenQueryer{}); err == nil {
		t.Error("LoadRules must fail when the rules read fails")
	}

	// Seed-write failure propagates: the fresh-table read finds nothing and
	// seeding the first row errors out.
	st := openRulesStore(t)
	if _, err := LoadRules(ctx, failExec{st}); err == nil {
		t.Error("LoadRules must fail when seeding a fresh table fails")
	}

	// A failing reload tick keeps the previous rules and keeps looping.
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}
	h := &recHandler{}
	stop := StartHotReload(r, brokenQueryer{}, 50*time.Millisecond, slog.New(h))
	t.Cleanup(stop)
	waitFor(t, func() bool { return h.saw("rules reload failed; keeping previous rules") }, 2*time.Second)
	if d := r.Handle(ctx, "/uptime"); !d.Handled || d.RuleID != "uptime" || d.Err != nil {
		t.Errorf("rules lost after failed reload tick: %+v", d)
	}
}
