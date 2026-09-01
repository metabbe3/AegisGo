package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
	"aegisgo/internal/trace"
)

// toolLLM is a fake whose responses optionally carry a function call —
// the tool-choice signal shadow comparison runs on.
type toolLLM struct {
	calls int
	tool  string // tool name the LLM "calls"; "" = none
}

func (f *toolLLM) RunText(_ context.Context, _ string, _ ...agent.Option) agent.ResponseStream {
	f.calls++
	var contents message.Contents
	if f.tool != "" {
		contents = append(contents, &message.FunctionCallContent{Name: f.tool, CallID: "c1"})
	}
	contents = append(contents, &message.TextContent{Text: "llm answer"})
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: contents}, nil)
	}
}

// shadowHarness builds an engine with a mined rule over csv_stats matching
// "summarize file <path>", its row inserted in the rules table at state.
func shadowHarness(t *testing.T, state string, llm LLMRunner) (*Engine, *store.Store) {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	set, err := tools.Builtin(tools.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	defs := append(router.Seeded(), router.RuleDef{
		Name: "mined_test", Pattern: `summarize file \S+`, Tool: "csv_stats",
		ArgsTemplate: `{"path":"testdata/sample.csv"}`, Origin: "mined", State: state,
	})
	rt, err := router.New(reg, defs)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Exec(context.Background(),
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts, state)
		 VALUES ('mined_test', 'summarize file \\S+', 'csv_stats', '{"path":"testdata/sample.csv"}', 'mined', 1, 't', ?)`,
		state); err != nil {
		t.Fatal(err)
	}
	return &Engine{Router: rt, LLM: llm, Store: st, IFace: store.IFaceCLI}, st
}

func ruleState(t *testing.T, st *store.Store, name string) (string, bool) {
	t.Helper()
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	var enabled int
	if err := st.QueryRow(context.Background(),
		`SELECT state, enabled FROM rules WHERE name=?`, name).Scan(&state, &enabled); err != nil {
		t.Fatal(err)
	}
	return state, enabled == 1
}

func TestShadowAgreementPromotesAfterStreak(t *testing.T) {
	old := PromoteAfter
	PromoteAfter = 3
	defer func() { PromoteAfter = old }()

	llm := &toolLLM{tool: "csv_stats"} // agrees: same tool
	e, st := shadowHarness(t, router.RuleShadow, llm)

	for i := 0; i < 3; i++ {
		_, ctx := trace.New(context.Background(), "")
		res := e.Run(ctx, "summarize file testdata/sample.csv")
		if res.DecisionSource != store.SourceLLM {
			t.Fatalf("shadow rule must not answer (run %d): %+v", i, res)
		}
	}
	if state, _ := ruleState(t, st, "mined_test"); state != store.RuleStateActive {
		t.Errorf("state = %q, want active after 3 agreements", state)
	}
	if n := countShadow(t, st); n != 3 {
		t.Errorf("shadow events = %d, want 3", n)
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d", llm.calls)
	}
}

func TestShadowDivergenceDemotes(t *testing.T) {
	llm := &toolLLM{tool: "read_csv"} // disagrees: different tool
	e, st := shadowHarness(t, router.RuleShadow, llm)

	_, ctx := trace.New(context.Background(), "")
	if res := e.Run(ctx, "summarize file testdata/sample.csv"); res.DecisionSource != store.SourceLLM {
		t.Fatalf("shadow rule must not answer: %+v", res)
	}
	state, enabled := ruleState(t, st, "mined_test")
	if state != store.RuleStateDemoted || enabled {
		t.Errorf("state=%q enabled=%v, want demoted+disabled", state, enabled)
	}
}

func TestSampledActiveRuleAnswersFromRule(t *testing.T) {
	prev := router.SampleForTest()
	router.SetSampleForTest(func() bool { return true }) // force the 1% branch
	defer router.SetSampleForTest(prev)

	llm := &toolLLM{tool: "other_tool"} // disagrees — but the rule still answers
	e, st := shadowHarness(t, router.RuleActive, llm)

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "summarize file testdata/sample.csv")
	if res.DecisionSource != store.SourceRouter {
		t.Fatalf("sampled active rule must still answer from the rule: %+v", res)
	}
	if state, _ := ruleState(t, st, "mined_test"); state != store.RuleStateDemoted {
		t.Errorf("state = %q, want demoted after sampled divergence", state)
	}
}

func TestToolNamesExtraction(t *testing.T) {
	llm := &toolLLM{tool: "read_csv"}
	resp, err := llm.RunText(context.Background(), "x").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := ToolNames(resp); len(got) != 1 || got[0] != "read_csv" {
		t.Errorf("ToolNames = %v", got)
	}

	llmNone := &toolLLM{}
	resp2, _ := llmNone.RunText(context.Background(), "x").Collect()
	if names := ToolNames(resp2); len(names) != 0 {
		t.Errorf("ToolNames(no calls) = %v", names)
	}
}

func TestRunStreamingRouterChunksAndLLMDeltas(t *testing.T) {
	e, _ := shadowHarness(t, router.RuleActive, nil)
	var got []string
	_, ctx := trace.New(context.Background(), "")
	// A long deterministic answer (8 CSV rows as JSON) spans several chunks.
	res := e.RunStreaming(ctx, "/csv_head testdata/sample.csv 8", func(c string) error {
		got = append(got, c)
		return nil
	})
	if res.DecisionSource != store.SourceRouter {
		t.Fatalf("res = %+v", res)
	}
	if len(got) < 2 {
		t.Errorf("expected chunked output, got %d chunks", len(got))
	}
	if joined := joinStrings(got); len(joined) != len(res.Answer) {
		t.Errorf("chunks lost text: %d vs %d", len(joined), len(res.Answer))
	}

	// LLM miss: deltas forwarded verbatim.
	e2 := &Engine{Router: e.Router, LLM: &toolLLM{tool: ""}, Store: e.Store}
	var deltas []string
	_, ctx2 := trace.New(context.Background(), "")
	res2 := e2.RunStreaming(ctx2, "what is the meaning of life", func(c string) error {
		deltas = append(deltas, c)
		return nil
	})
	if res2.DecisionSource != store.SourceLLM || joinStrings(deltas) != res2.Answer {
		t.Errorf("deltas = %q answer = %q", joinStrings(deltas), res2.Answer)
	}
}

func countShadow(t *testing.T, st *store.Store) int {
	t.Helper()
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM shadow_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func joinStrings(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s
	}
	return out
}
