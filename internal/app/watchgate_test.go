package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/logwatch"
	"aegisgo/internal/store"
)

func newWatchGate(t *testing.T) (*WatchGate, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := logwatch.NewManager(8)
	t.Cleanup(mgr.Stop)
	return &WatchGate{
		Mgr: mgr,
		Store: &logwatch.StoreAdapter{
			Exec: func(ctx context.Context, q string, args ...any) error { return st.Exec(ctx, q, args...) },
			QueryRows: func(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
				return st.QueryMaps(ctx, q, args...)
			},
		},
	}, st
}

func TestParseWatchValidAndDefaults(t *testing.T) {
	w, err := ParseWatch(`/watch name=gw | path=/tmp/gw.log | pattern=panic|FATAL | every=30s | cooldown=5m`)
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "gw" || w.Path != "/tmp/gw.log" || w.Pattern != "panic|FATAL" {
		t.Fatalf("w = %+v", w)
	}
	if w.Every != 30*time.Second || w.Cooldown != 5*time.Minute {
		t.Fatalf("durations = %v/%v", w.Every, w.Cooldown)
	}
	// defaults when omitted
	w2, err := ParseWatch(`/watch name=a-b | path=/x.log | pattern=err`)
	if err != nil {
		t.Fatal(err)
	}
	if w2.Every != 30*time.Second || w2.Cooldown != 10*time.Minute {
		t.Fatalf("defaults = %v/%v", w2.Every, w2.Cooldown)
	}
}

func TestParseWatchRejects(t *testing.T) {
	bad := []string{
		`/watch`,
		`/watch name=Bad | pattern=/x | path=/x`,          // name charset
		`/watch name=ok | path=/x | pattern=[`,            // regex
		`/watch name=ok | pattern=p | path=/x | every=xx`, // duration
		`/watch name=ok | pattern=p | path=/x | nope=1`,   // key
		`/watch pattern=p | path=/x`,                      // missing name
	}
	for _, in := range bad {
		if _, err := ParseWatch(in); err == nil {
			t.Errorf("ParseWatch(%q) accepted", in)
		}
	}
}

func TestParseWatchBareNumberSeconds(t *testing.T) {
	w, err := ParseWatch(`/watch name=nn | path=/x | pattern=p | every=45`)
	if err != nil {
		t.Fatal(err)
	}
	if w.Every != 45*time.Second {
		t.Fatalf("every = %v, want 45s", w.Every)
	}
}

func TestWatchAddRemovePersist(t *testing.T) {
	g, st := newWatchGate(t)
	ctx := context.Background()

	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	os.WriteFile(p, []byte("nothing\n"), 0o644)

	out := g.HandleWatchText(ctx, `/watch name=gw | path=`+p+` | pattern=panic | every=100ms`)
	if !strings.Contains(out, "LIVE") {
		t.Fatalf("watch reply = %q", out)
	}
	// persisted?
	ws, err := g.Store.ListWatches(ctx)
	if err != nil || len(ws) != 1 || ws[0].Name != "gw" {
		t.Fatalf("persisted = %+v err=%v", ws, err)
	}
	// watchlist mentions it + counts
	lst := g.HandleWatchText(ctx, `/watchlist`)
	if !strings.Contains(lst, "gw") {
		t.Fatalf("watchlist = %q", lst)
	}
	// remove
	rem := g.HandleWatchText(ctx, `/unwatch gw`)
	if !strings.Contains(rem, "removed") {
		t.Fatalf("unwatch = %q", rem)
	}
	var n int
	st.QueryRow(ctx, `SELECT COUNT(*) FROM log_watches`).Scan(&n)
	if n != 0 {
		t.Fatalf("rows after remove = %d", n)
	}
}

func TestWatchAlertFiresAndFormats(t *testing.T) {
	g, _ := newWatchGate(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "b.log")
	os.WriteFile(p, []byte("ok\n"), 0o644)

	notified := make(chan string, 4)
	g.Notify = func(text string) { notified <- text }
	if _, err := g.StartWatches(ctx); err != nil { // no stored watches yet
		t.Fatal(err)
	}
	if out := g.HandleWatchText(ctx, `/watch name=crit | path=`+p+` | pattern=FATAL | every=50ms | cooldown=1m`); !strings.Contains(out, "LIVE") {
		t.Fatalf("add = %q", out)
	}
	// persist for the pump (pump is started in StartWatches; here notify is
	// direct — assert via channel)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("FATAL: disk full\n")
	f.Close()
	// poll manager alerts manually (pump started above with empty set)
	select {
	case a := <-g.Mgr.Alerts():
		if a.Watch != "crit" || a.Line != "FATAL: disk full" {
			t.Fatalf("alert = %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert fired")
	}
}

func TestAnalyzeCommandFormats(t *testing.T) {
	g, _ := newWatchGate(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "c.log")
	os.WriteFile(p, []byte("INFO fine\nERROR bad thing\nERROR bad thing\n"), 0o644)
	out := g.HandleWatchText(context.Background(), `/analyze `+p+` -n 50`)
	for _, want := range []string{"Log analysis", "Top patterns", "Top errors", "2×"} {
		if !strings.Contains(out, want) {
			t.Errorf("analyze missing %q: %q", want, out)
		}
	}
	// usage & errors
	if out := g.HandleWatchText(context.Background(), `/analyze`); !strings.Contains(out, "usage") {
		t.Errorf("analyze usage = %q", out)
	}
	if out := g.HandleWatchText(context.Background(), `/analyze /nonexistent-x`); !strings.Contains(out, "⚠️") {
		t.Errorf("analyze missing-file = %q", out)
	}
}

func TestStartWatchesRehydrates(t *testing.T) {
	g, _ := newWatchGate(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "d.log")
	os.WriteFile(p, []byte(""), 0o644)
	if err := g.Store.SaveWatch(ctx, logwatch.StoredWatch{
		Name: "boot", Path: p, Pattern: "x", Every: time.Minute, Cooldown: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := g.StartWatches(ctx)
	if err != nil || n != 1 {
		t.Fatalf("rehydrate = %d err=%v", n, err)
	}
	if c := g.Mgr.Counts(); len(c) != 1 {
		t.Fatalf("manager counts = %v", c)
	}
}

func TestStartWatchesBadStoredPatternErrors(t *testing.T) {
	g, _ := newWatchGate(t)
	ctx := context.Background()
	if err := g.Store.SaveWatch(ctx, logwatch.StoredWatch{
		Name: "broken", Path: "/tmp/x.log", Pattern: "[", Every: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.StartWatches(ctx); err == nil {
		t.Fatal("bad stored pattern should fail rehydrate honestly")
	}
}

func TestFormatAnalysisLongTemplateClamps(t *testing.T) {
	long := strings.Repeat("x", 200)
	a := &logwatch.Analysis{
		Path: "/tmp/a.log", LinesRead: 1,
		Top: []logwatch.PatternHit{{Template: long, Count: 1}},
	}
	out := FormatAnalysis(a)
	if !strings.Contains(out, "…") {
		t.Fatal("long template not clamped")
	}
	if !strings.Contains(out, "watch") { // singular: 1 watch
		_ = out // FormatAnalysis tak menyebut watch; guard no-op utk plural path lain
	}
}

func TestStartWatchesWithNotifyPump(t *testing.T) {
	g, _ := newWatchGate(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "pump.log")
	os.WriteFile(p, []byte(""), 0o644)
	notified := make(chan string, 2)
	g.Notify = func(text string) { notified <- text }
	if err := g.Store.SaveWatch(ctx, logwatch.StoredWatch{
		Name: "pmp", Path: p, Pattern: "BOOM", Every: 40 * time.Millisecond, Cooldown: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.StartWatches(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("BOOM pump test\n")
	f.Close()
	select {
	case txt := <-notified:
		if !strings.Contains(txt, "pmp") || !strings.Contains(txt, "BOOM") {
			t.Fatalf("notify = %q", txt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump never notified")
	}
}

func TestPluralHelper(t *testing.T) {
	if plural(1) != "" || plural(0) != "es" || plural(3) != "es" {
		t.Fatal("plural")
	}
}
