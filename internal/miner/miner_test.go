package miner

import (
	"context"
	"regexp"
	"testing"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
)

func seedCorpus(t *testing.T, st *store.Store, shape, tools string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		st.RecordFallback(store.FallbackEvent{
			TraceID: shape, NormalizedPrompt: shape, RawPrompt: shape, ToolsUsed: tools,
		})
	}
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond every ~10ms until it holds or the deadline passes.
// Polling keeps these tests deterministic under slow or race-instrumented
// builds where fixed sleeps would flake.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestMineProposesShadowRule(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Eligible: big cluster, dominant derivable tool, exactly one <path>.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 20)
	// Ineligible: no path slot.
	seedCorpus(t, st, "how many rows", "read_csv", 30)
	// Ineligible: non-derivable tool.
	seedCorpus(t, st, "check <path> system", "system_command", 30)
	// Ineligible: below threshold.
	seedCorpus(t, st, "peek <path>", "read_doc", 2)
	// Ineligible: mixed tools (dominance < 0.85).
	seedCorpus(t, st, "scan <path> now", "read_csv", 10)
	seedCorpus(t, st, "scan <path> now", "csv_stats", 10)

	proposals, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v, want exactly 1", proposals)
	}
	p := proposals[0]
	if p.Tool != "csv_stats" || p.ArgsTemplate != `{"path":"$1"}` {
		t.Errorf("proposal = %+v", p)
	}
	want := `summarize\s+(\S+)\s+quickly`
	if p.Pattern != want {
		t.Errorf("pattern = %q, want %q", p.Pattern, want)
	}

	// The compiled pattern actually matches a real prompt shape and the
	// spliced args carry the capture.
	compiled, err := router.RuleDef{
		Name: p.Name, Pattern: p.Pattern, Tool: p.Tool,
		ArgsTemplate: p.ArgsTemplate, Origin: "mined", State: router.RuleShadow,
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	m := compiled.Regexp().FindStringSubmatch("summarize testdata/sample.csv quickly")
	if m == nil || m[1] != "testdata/sample.csv" {
		t.Errorf("match = %v", m)
	}

	// Row landed as shadow.
	var state string
	if err := st.QueryRow(ctx,
		`SELECT state FROM rules WHERE name=?`, p.Name).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != router.RuleShadow {
		t.Errorf("state = %q, want shadow", state)
	}

	// Idempotent: second pass proposes nothing.
	again, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second pass = %+v, want none", again)
	}
}

func TestMineNeverDuplicatesSeededPattern(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	ctx := context.Background()

	// Seed the rules table first (as app boot does), then feed a cluster
	// whose synthesized pattern equals the seeded csv_summary pattern: the
	// miner must skip, not duplicate.
	if _, err := router.LoadRules(ctx, st); err != nil {
		t.Fatal(err)
	}
	seedCorpus(t, st, "/csv_summary\\s+(\\S+)", "csv_stats", 20)
	if _, err := Mine(ctx, st, Options{Threshold: 20}, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.QueryRow(ctx,
		`SELECT COUNT(*) FROM rules WHERE pattern=?`, `/csv_summary\s+(\S+)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rules with seeded pattern = %d, want 1 (no duplicate)", n)
	}
}

func TestStartDisabled(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Eligible cluster: if a disabled miner ever ran a pass, a mined rule
	// would land immediately. Reality: interval <= 0 makes Start return a
	// plain no-op WITHOUT spawning the loop goroutine, so no pass ever runs
	// and the immediate count below is deterministic.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 20)

	stop := Start(ctx, st, Options{Threshold: 20}, 0, nil)
	if stop == nil {
		t.Fatal("Start(interval=0) returned a nil stop func")
	}
	stop() // must not block or panic
	stop() // the disabled stop is a no-op closure: a second call is safe

	var n int
	if err := st.QueryRow(ctx,
		`SELECT COUNT(*) FROM rules WHERE name LIKE 'mined_%' OR state=?`,
		router.RuleShadow).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("mined/shadow rule rows = %d, want 0 (interval<=0 never mines)", n)
	}
}

func TestStartMinesPeriodically(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Threshold-eligible shape: the loop's first tick must mine it.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 2)

	// stop's idempotence is Start's contract (like store.Close), so the
	// deferred call only guards the t.Fatal paths.
	stop := Start(context.Background(), st, Options{Threshold: 2},
		100*time.Millisecond, nil)
	defer stop()

	var gotName, gotState, gotPattern string
	var gotEnabled int
	ok := waitFor(t, 2*time.Second, func() bool {
		var name, state, pattern string
		var enabled int
		err := st.QueryRow(context.Background(),
			`SELECT name, state, pattern, enabled FROM rules
			 WHERE name LIKE 'mined_%' AND state=?`,
			router.RuleShadow).Scan(&name, &state, &pattern, &enabled)
		if err == nil {
			gotName, gotState, gotPattern, gotEnabled = name, state, pattern, enabled
		}
		return err == nil
	})
	if !ok {
		t.Fatal("periodic mining loop never inserted a mined shadow rule")
	}
	if gotName != "mined_1" {
		t.Errorf("name = %q, want mined_1", gotName)
	}
	if gotState != router.RuleShadow {
		t.Errorf("state = %q, want %q", gotState, router.RuleShadow)
	}
	if gotEnabled != 1 {
		t.Errorf("enabled = %d, want 1", gotEnabled)
	}
	if want := `summarize\s+(\S+)\s+quickly`; gotPattern != want {
		t.Errorf("pattern = %q, want %q", gotPattern, want)
	}

	// Double-stop must not panic: cleanup closures legitimately fire from
	// more than one exit path (e.g. app.Build's error branch and the
	// caller's defer), so Start guards close(done) with a sync.Once.
	stop()
	stop()
}

func TestStartStopsOnContextCancel(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Threshold-eligible shape: one tick would mine it.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 2)

	// A canceled boot context must end the loop before the first tick
	// fires: 300ms at a 100ms interval is three missed ticks, so a mined
	// rule can only appear if the loop ignored ctx and kept ticking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop := Start(ctx, st, Options{Threshold: 2}, 100*time.Millisecond, nil)
	defer stop()

	if waitFor(t, 300*time.Millisecond, func() bool {
		var n int
		if err := st.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM rules WHERE name LIKE 'mined_%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}) {
		t.Error("mining loop ran a pass after ctx was canceled")
	}
}

func TestMineDefaultThreshold(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Zero Options falls back to the default threshold of 20 events per
	// shape: 19 rows must stay below it.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 19)
	proposals, err := Mine(ctx, st, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Errorf("proposals at 19 rows = %+v, want none", proposals)
	}
	var n int
	if err := st.QueryRow(ctx, `SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rules rows at 19 rows = %d, want 0", n)
	}

	// The 20th event crosses the default: exactly one proposal appears.
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 1)
	proposals, err = Mine(ctx, st, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals at 20 rows = %+v, want exactly 1", proposals)
	}
	if proposals[0].ClusterSize != 20 {
		t.Errorf("ClusterSize = %d, want 20", proposals[0].ClusterSize)
	}
}

func TestMineInsertsShadowRow(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 20)
	proposals, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v, want exactly 1", proposals)
	}

	var name, pattern, tool, argsTemplate, origin, state string
	var enabled int
	if err := st.QueryRow(ctx,
		`SELECT name, pattern, tool, args_template, origin, enabled, state
		 FROM rules WHERE name=?`, proposals[0].Name).Scan(
		&name, &pattern, &tool, &argsTemplate, &origin, &enabled, &state); err != nil {
		t.Fatal(err)
	}
	if name != "mined_1" {
		t.Errorf("name = %q, want mined_1", name)
	}
	if want := `summarize\s+(\S+)\s+quickly`; pattern != want {
		t.Errorf("pattern = %q, want %q", pattern, want)
	}
	if tool != "csv_stats" {
		t.Errorf("tool = %q, want csv_stats", tool)
	}
	if argsTemplate != `{"path":"$1"}` {
		t.Errorf("args_template = %q, want {\"path\":\"$1\"}", argsTemplate)
	}
	if origin != "mined" {
		t.Errorf("origin = %q, want mined", origin)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d, want 1 (shadow rules enter live)", enabled)
	}
	if state != router.RuleShadow {
		t.Errorf("state = %q, want %q", state, router.RuleShadow)
	}
}

func TestMineSkipsExistingPattern(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Pre-insert a rule carrying the exact pattern Mine would synthesize:
	// the exists>0 branch must skip silently — no proposal, no error, and
	// no second row.
	if err := st.Exec(ctx,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts, state)
		 VALUES (?,?,?,?,?,1,?,?)`,
		"csv_summary_like", `summarize\s+(\S+)\s+quickly`, "read_doc",
		`{"path":"$1"}`, "seeded",
		time.Now().UTC().Format(time.RFC3339Nano), router.RuleActive); err != nil {
		t.Fatal(err)
	}
	seedCorpus(t, st, "summarize <path> quickly", "csv_stats", 20)

	proposals, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatalf("Mine with existing pattern: %v", err)
	}
	if len(proposals) != 0 {
		t.Errorf("proposals = %+v, want none (pattern already present)", proposals)
	}
	var n int
	if err := st.QueryRow(ctx,
		`SELECT COUNT(*) FROM rules WHERE pattern=?`,
		`summarize\s+(\S+)\s+quickly`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rules rows with pattern = %d, want 1 (no duplicate)", n)
	}
	if err := st.QueryRow(ctx,
		`SELECT COUNT(*) FROM rules WHERE origin='mined'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("mined rules = %d, want 0", n)
	}
}

func TestDominantToolNullAndMixed(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// NULL tools_used rows still count toward the cluster size, so a
	// non-null split of 7/5 under 8 NULLs tops out at 7/20 = 0.35, far
	// below the 0.85 dominance bar.
	seedCorpus(t, st, "scan <path> now", "", 8)
	seedCorpus(t, st, "scan <path> now", "read_csv", 7)
	seedCorpus(t, st, "scan <path> now", "csv_stats", 5)
	if tool, ok := dominantTool(ctx, st, "scan <path> now", 20); ok {
		t.Errorf("mixed cluster dominantTool = (%q, true), want none", tool)
	}

	// An all-NULL cluster has no tool choice at all.
	seedCorpus(t, st, "blank <path>", "", 20)
	if tool, ok := dominantTool(ctx, st, "blank <path>", 20); ok {
		t.Errorf("all-NULL dominantTool = (%q, true), want none", tool)
	}

	// Even a live 10/10 split (no NULLs) is non-dominant: 0.5 < 0.85.
	seedCorpus(t, st, "even <path> split", "read_csv", 10)
	seedCorpus(t, st, "even <path> split", "csv_stats", 10)
	if tool, ok := dominantTool(ctx, st, "even <path> split", 20); ok {
		t.Errorf("50/50 dominantTool = (%q, true), want none", tool)
	}

	proposals, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Errorf("proposals = %+v, want none", proposals)
	}
	var n int
	if err := st.QueryRow(ctx, `SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rules rows = %d, want 0", n)
	}
}

func TestDominantToolOnlyPathArgTools(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Mining law: only path-arg tools are derivable. Even a 100%-dominant
	// cluster of system_command or sql_query runs must never auto-mine —
	// system_command input must never reach argv and sql_query args are
	// free-form.
	seedCorpus(t, st, "run <path> now", "system_command", 20)
	seedCorpus(t, st, "query <path> now", "sql_query", 20)
	// A comma-joined multi-tool string is not a single derivable choice.
	seedCorpus(t, st, "both <path>", "read_csv,csv_stats", 20)

	for _, shape := range []string{"run <path> now", "query <path> now", "both <path>"} {
		if tool, ok := dominantTool(ctx, st, shape, 20); ok {
			t.Errorf("dominantTool(%q) = (%q, true), want none", shape, tool)
		}
	}

	proposals, err := Mine(ctx, st, Options{Threshold: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Errorf("proposals = %+v, want none (non-derivable tools never mine)", proposals)
	}
	var n int
	if err := st.QueryRow(ctx, `SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rules rows = %d, want 0", n)
	}
}

func TestSynthesizePatternVariants(t *testing.T) {
	cases := []struct {
		shape string
		want  string
		exact string // a concrete prompt the pattern must match
	}{
		{"summarize <path> quickly", `summarize\s+(\S+)\s+quickly`, "summarize a.csv quickly"},
		{"top <n> rows in <path>", `top\s+\d+\s+rows\s+in\s+(\S+)`, "top 5 rows in b.csv"},
		{`find <q> in <path>`, `find\s+"[^"]*"\s+in\s+(\S+)`, `find "revenue" in c.csv`},
		{"summarize <path> now?", `summarize\s+(\S+)\s+now\?`, "summarize d.csv now?"},
	}
	for _, tc := range cases {
		got, ok := synthesizePattern(tc.shape)
		if !ok || got != tc.want {
			t.Errorf("synthesizePattern(%q) = (%q, %v), want (%q, true)", tc.shape, got, ok, tc.want)
			continue
		}
		re, err := regexp.Compile(got)
		if err != nil {
			t.Errorf("pattern %q does not compile: %v", got, err)
			continue
		}
		m := re.FindStringSubmatch(tc.exact)
		if m == nil {
			t.Errorf("pattern %q does not match %q", got, tc.exact)
		}
	}

	// Exactly one <path> slot is required: zero or two never synthesizes. A
	// placeholder glued to punctuation ("<path>?") is not the <path> token
	// either, so such a shape is simply unminable.
	for _, bad := range []string{"how many rows", "copy <path> to <path>", "what's in <path>?"} {
		if got, ok := synthesizePattern(bad); ok {
			t.Errorf("synthesizePattern(%q) = (%q, true), want ok=false", bad, got)
		}
	}
}

func TestMineQueryError(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A dead context surfaces as an error from the cluster query.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Mine(ctx, st, Options{Threshold: 1}, nil); err == nil {
		t.Error("Mine with canceled context returned nil error")
	}

	// dominantTool folds the same failure into "no dominant tool" — a
	// mining pass degrades to skipping the cluster, never to a panic.
	if tool, ok := dominantTool(ctx, st, "anything", 5); ok || tool != "" {
		t.Errorf("dominantTool with canceled context = (%q, %v), want (\"\", false)", tool, ok)
	}
}
