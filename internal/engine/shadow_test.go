package engine

import (
	"context"
	"errors"
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

func TestRunStreamingSampledRuleCompares(t *testing.T) {
	prev := router.SampleForTest()
	router.SetSampleForTest(func() bool { return true }) // force the 1% branch
	defer router.SetSampleForTest(prev)

	llm := &toolLLM{tool: "csv_stats"} // agrees: same tool the rule runs
	e, st := shadowHarness(t, router.RuleActive, llm)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("sampled active rule must answer from the rule: %+v", res)
	}
	if llm.calls != 1 {
		t.Errorf("llm calls = %d, want exactly one comparison run", llm.calls)
	}
	// The inline comparison landed: tool-choice agreement against the rule.
	if n := countRows(t, st, `SELECT COUNT(*) FROM shadow_events
		WHERE rule_name='mined_test' AND agreed=1 AND llm_tools='csv_stats'`); n != 1 {
		t.Errorf("agreed shadow rows = %d, want 1", n)
	}
	if state, enabled := ruleState(t, st, "mined_test"); state != store.RuleStateActive || !enabled {
		t.Errorf("agreement must keep the rule active: state=%q enabled=%v", state, enabled)
	}
	if joinStrings(chunks) != res.Answer || len(chunks) == 0 {
		t.Errorf("streamed %d chunks, answer %q", len(chunks), res.Answer)
	}
}

func TestRunStreamingSampledRuleLLMError(t *testing.T) {
	prev := router.SampleForTest()
	router.SetSampleForTest(func() bool { return true })
	defer router.SetSampleForTest(prev)

	e, st := shadowHarness(t, router.RuleActive, &errLLM{err: errors.New("provider exploded")})

	var emitted int
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(string) error {
		emitted++
		return nil
	})
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("the rule's answer must survive the comparison failing: %+v", res)
	}
	if emitted == 0 {
		t.Error("emit was never called")
	}
	// A comparison that cannot run is not a disagreement: no shadow event,
	// no lifecycle change — the rule answers on untouched.
	if n := countShadow(t, st); n != 0 {
		t.Errorf("shadow rows = %d, want 0 (comparison unavailable)", n)
	}
	if state, enabled := ruleState(t, st, "mined_test"); state != store.RuleStateActive || !enabled {
		t.Errorf("state=%q enabled=%v, want active untouched", state, enabled)
	}
}

func TestShadowLLMErrorFallsBackToRule(t *testing.T) {
	e, st := shadowHarness(t, router.RuleShadow, &errLLM{err: errors.New("provider exploded")})

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "summarize file testdata/sample.csv")
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("comparison unavailable: the rule must answer: %+v", res)
	}
	if res.Answer == "" {
		t.Error("rule answer is empty")
	}
	if n := countShadow(t, st); n != 0 {
		t.Errorf("shadow rows = %d, want 0", n)
	}
	if state, enabled := ruleState(t, st, "mined_test"); state != store.RuleStateShadow || !enabled {
		t.Errorf("state=%q enabled=%v, want shadow untouched (no comparison, no transition)", state, enabled)
	}
	iface, source, outcome, rule := lastAudit(t, st)
	if iface != store.IFaceCLI || source != store.SourceRouter || outcome != "ok" || rule != "mined_test" {
		t.Errorf("audit = (%q, %q, %q, %q)", iface, source, outcome, rule)
	}
}

func TestRunStreamingShadowRuleAnswersViaLLM(t *testing.T) {
	llm := &toolLLM{tool: "csv_stats"} // agrees — but shadow rules never answer
	e, st := shadowHarness(t, router.RuleShadow, llm)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceLLM || res.RuleID != "mined_test" {
		t.Fatalf("shadow state must defer to the LLM: %+v", res)
	}
	if res.Answer != "llm answer" {
		t.Errorf("answer = %q", res.Answer)
	}
	// Shadow answers emit once, synchronously — no token-delta forwarding.
	if len(chunks) != 1 || chunks[0] != res.Answer {
		t.Errorf("chunks = %q, want the whole answer in one chunk", chunks)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM shadow_events WHERE rule_name='mined_test' AND agreed=1`); n != 1 {
		t.Errorf("agreed shadow rows = %d, want 1", n)
	}
}

func TestLLMOffShadowRuleStillAnswers(t *testing.T) {
	e, st := shadowHarness(t, router.RuleShadow, nil)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	// The rule asked for an evaluation the engine cannot run; answering from
	// the rule is never worse than the disabled fallback.
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("res = %+v", res)
	}
	if joinStrings(chunks) != res.Answer || len(chunks) == 0 {
		t.Errorf("chunks = %d, answer %q", len(chunks), res.Answer)
	}
	if n := countShadow(t, st); n != 0 {
		t.Errorf("no LLM, no comparison: shadow rows = %d", n)
	}
}

// TestSampledRuleLLMOffStillAnswers: the sampling flag fired but the LLM is
// disabled — the inline comparison is skipped and the rule just answers.
func TestSampledRuleLLMOffStillAnswers(t *testing.T) {
	prev := router.SampleForTest()
	router.SetSampleForTest(func() bool { return true })
	defer router.SetSampleForTest(prev)

	e, st := shadowHarness(t, router.RuleActive, nil)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("res = %+v", res)
	}
	if joinStrings(chunks) != res.Answer || len(chunks) == 0 {
		t.Errorf("chunks = %d, answer %q", len(chunks), res.Answer)
	}
	if n := countShadow(t, st); n != 0 {
		t.Errorf("no LLM, no comparison: shadow rows = %d", n)
	}
}

// TestShadowStoreFailuresAreNonFatal proves a store that stops accepting
// writes never takes the request path down: the comparison, demotion, and
// streak read all log their failures and the run still completes.
func TestShadowStoreFailuresAreNonFatal(t *testing.T) {
	llm := &toolLLM{tool: "read_csv"} // first run disagrees, then agrees
	e, st := shadowHarness(t, router.RuleShadow, llm)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, ctx := trace.New(context.Background(), "")
	if res := e.Run(ctx, "summarize file testdata/sample.csv"); res.DecisionSource != store.SourceLLM {
		t.Fatalf("divergence run: res = %+v", res)
	}

	llm.tool = "csv_stats" // agreement path: the streak read now fails too
	_, ctx2 := trace.New(context.Background(), "")
	if res := e.Run(ctx2, "summarize file testdata/sample.csv"); res.DecisionSource != store.SourceLLM {
		t.Fatalf("agreement run: res = %+v", res)
	}
}

// TestShadowPromoteWriteFailureKeepsServing: a full agreeing streak whose
// promotion UPDATE fails (rules table gone) must not fail the run — the
// comparisons are still recorded and the LLM still answers.
func TestShadowPromoteWriteFailureKeepsServing(t *testing.T) {
	old := PromoteAfter
	PromoteAfter = 2
	defer func() { PromoteAfter = old }()

	llm := &toolLLM{tool: "csv_stats"}
	e, st := shadowHarness(t, router.RuleShadow, llm)
	if err := st.Exec(context.Background(), `DROP TABLE rules`); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		_, ctx := trace.New(context.Background(), "")
		if res := e.Run(ctx, "summarize file testdata/sample.csv"); res.DecisionSource != store.SourceLLM {
			t.Fatalf("run %d: res = %+v", i, res)
		}
	}
	if n := countShadow(t, st); n != 2 {
		t.Errorf("shadow rows = %d, want both comparisons recorded despite the failed promotion", n)
	}
}

// TestSampledCompareStoreFailureStillAnswers: the inline comparison cannot
// record (store closed) — the sampled active rule still answers from the rule.
func TestSampledCompareStoreFailureStillAnswers(t *testing.T) {
	prev := router.SampleForTest()
	router.SetSampleForTest(func() bool { return true })
	defer router.SetSampleForTest(prev)

	e, st := shadowHarness(t, router.RuleActive, &toolLLM{tool: "csv_stats"})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var emitted int
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize file testdata/sample.csv", func(string) error {
		emitted++
		return nil
	})
	if res.DecisionSource != store.SourceRouter || res.RuleID != "mined_test" {
		t.Fatalf("res = %+v", res)
	}
	if emitted == 0 {
		t.Error("emit was never called")
	}
}
