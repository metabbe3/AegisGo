package miner

import (
	"context"
	"testing"

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
	st, _ := store.Open(":memory:")
	defer st.Close()
	stop := Start(context.Background(), st, Options{}, 0, nil)
	stop() // must not block or panic
}
