package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
	"aegisgo/internal/trace"
)

// fakeLLM returns a canned answer, recording the prompts it saw.
type fakeLLM struct {
	prompts []string
}

func (f *fakeLLM) RunText(_ context.Context, msg string, _ ...agent.Option) agent.ResponseStream {
	f.prompts = append(f.prompts, msg)
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		yield(&agent.ResponseUpdate{
			Role:     message.RoleAssistant,
			Contents: message.Contents{&message.TextContent{Text: "llm answer"}},
		}, nil)
	}
}

func newEngine(t *testing.T, llm LLMRunner) (*Engine, *store.Store) {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	// engine tests run from internal/engine; workspace = repo root
	root := filepath.Dir(filepath.Dir(wd))
	set, err := tools.Builtin(tools.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := router.New(reg, router.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{Router: r, LLM: llm, Store: st, IFace: store.IFaceCLI}, st
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	// Audit/fallback writes are batched; barrier before reading.
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func TestEngineRouterHitSkipsLLM(t *testing.T) {
	fake := &fakeLLM{}
	e, st := newEngine(t, fake)

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "/hostname")
	if res.DecisionSource != store.SourceRouter {
		t.Fatalf("source = %q", res.DecisionSource)
	}
	if len(fake.prompts) != 0 {
		t.Errorf("LLM was called %d times on a router hit", len(fake.prompts))
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_events WHERE decision_source=?`, store.SourceRouter); n != 1 {
		t.Errorf("router audit rows = %d, want 1", n)
	}
}

func TestEngineLLMFallback(t *testing.T) {
	fake := &fakeLLM{}
	e, st := newEngine(t, fake)

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "tell me a story")
	if res.DecisionSource != store.SourceLLM || res.Answer != "llm answer" {
		t.Fatalf("res = %+v", res)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("LLM calls = %d, want 1", len(fake.prompts))
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events`); n != 1 {
		t.Errorf("fallback corpus rows = %d, want 1", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_events WHERE decision_source=?`, store.SourceLLM); n != 1 {
		t.Errorf("llm audit rows = %d", n)
	}
}

func TestEngineKillSwitch(t *testing.T) {
	e, st := newEngine(t, nil) // AEGIS_LLM=off

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "tell me a story")
	if res.DecisionSource != store.SourceLLMOff {
		t.Fatalf("source = %q, want llm_disabled", res.DecisionSource)
	}
	if res.Answer == "" {
		t.Error("kill switch should still answer something")
	}
	// Router commands keep working with the LLM off — the survivability point.
	_, rctx := trace.New(context.Background(), "")
	if res := e.Run(rctx, "/uptime"); res.DecisionSource != store.SourceRouter {
		t.Fatalf("router broken with LLM off: %+v", res)
	}
	var traceID string
	if err := st.QueryRow(context.Background(), `SELECT trace_id FROM audit_events LIMIT 1`).Scan(&traceID); err != nil {
		t.Fatal(err)
	}
	if traceID == "" {
		t.Error("audit rows missing trace_id")
	}
}

// errLLM streams one text delta, then fails mid-stream — the provider-outage
// shape both Run (via Collect) and RunStreaming (delta loop) must survive.
type errLLM struct {
	err     error
	prompts []string
}

func (f *errLLM) RunText(_ context.Context, msg string, _ ...agent.Option) agent.ResponseStream {
	f.prompts = append(f.prompts, msg)
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		if !yield(&agent.ResponseUpdate{
			Role:     message.RoleAssistant,
			Contents: message.Contents{&message.TextContent{Text: "partial answer"}},
		}, nil) {
			return
		}
		yield(nil, f.err)
	}
}

// streamLLM yields a configurable update sequence — text deltas, one function
// call on the first delta, one usage content on the last, optionally a bare
// nil update first — recording every prompt. It honors the iterator
// contract: once the consumer breaks, no further updates are produced,
// exactly like a provider stream the caller stopped reading.
type streamLLM struct {
	prompts []string
	deltas  []string             // one update per delta, yielded in order
	tool    string               // function call attached to the first delta
	usage   message.UsageDetails // attached to the last delta when counts are set
	leadNil bool                 // yield a payload-less nil update first
}

func (f *streamLLM) RunText(_ context.Context, msg string, _ ...agent.Option) agent.ResponseStream {
	f.prompts = append(f.prompts, msg)
	var updates []*agent.ResponseUpdate
	if f.leadNil {
		updates = append(updates, nil)
	}
	for i, d := range f.deltas {
		upd := &agent.ResponseUpdate{
			Role:     message.RoleAssistant,
			Contents: message.Contents{&message.TextContent{Text: d}},
		}
		if i == 0 && f.tool != "" {
			upd.Contents = append(upd.Contents, &message.FunctionCallContent{Name: f.tool, CallID: "c1"})
		}
		last := i == len(f.deltas)-1
		if last && (f.usage.InputTokenCount != 0 || f.usage.OutputTokenCount != 0) {
			upd.Contents = append(upd.Contents, &message.UsageContent{Details: f.usage})
		}
		updates = append(updates, upd)
	}
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		for _, u := range updates {
			if !yield(u, nil) {
				return
			}
		}
	}
}

// lastAudit returns the newest audit row's nullable fields (NULL reads as "").
func lastAudit(t *testing.T, st *store.Store) (iface, source, outcome, rule string) {
	t.Helper()
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ruleCol sql.NullString
	if err := st.QueryRow(context.Background(),
		`SELECT interface, decision_source, outcome, rule_id FROM audit_events ORDER BY id DESC LIMIT 1`).
		Scan(&iface, &source, &outcome, &ruleCol); err != nil {
		t.Fatal(err)
	}
	return iface, source, outcome, ruleCol.String
}

func TestWithIFaceOverridesEngineDefault(t *testing.T) {
	e, st := newEngine(t, nil) // engine default interface: cli
	_, base := trace.New(context.Background(), "")

	if res := e.Run(WithIFace(base, store.IFaceREST), "/hostname"); res.DecisionSource != store.SourceRouter {
		t.Fatalf("res = %+v", res)
	}
	if iface, _, _, _ := lastAudit(t, st); iface != store.IFaceREST {
		t.Errorf("interface = %q, want rest (ctx value must win over the engine field)", iface)
	}

	// No ctx value: the engine's own default labels the row.
	if res := e.Run(base, "/hostname"); res.DecisionSource != store.SourceRouter {
		t.Fatalf("res = %+v", res)
	}
	if iface, _, _, _ := lastAudit(t, st); iface != store.IFaceCLI {
		t.Errorf("interface = %q, want the engine default cli", iface)
	}

	// A pre-set Interface is never relabeled by ctx or engine default.
	e.audit(base, store.AuditEvent{TraceID: "t-iface", Interface: store.IFaceGRPC,
		DecisionSource: store.SourceRouter})
	if iface, _, _, _ := lastAudit(t, st); iface != store.IFaceGRPC {
		t.Errorf("interface = %q, want the pre-set grpc label kept", iface)
	}
}

func TestRunStreamingRouterChunks(t *testing.T) {
	fake := &fakeLLM{}
	e, st := newEngine(t, fake)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	// csv_summary over the sample fixture returns a stats JSON well past the
	// 256-byte chunk size, forcing the multi-chunk deterministic path.
	res := e.RunStreaming(ctx, "/csv_summary testdata/sample.csv", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceRouter || res.RuleID != "csv_summary" {
		t.Fatalf("res = %+v", res)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2 (answer is %d bytes)", len(chunks), len(res.Answer))
	}
	for i, c := range chunks[:len(chunks)-1] {
		if len(c) != streamChunkLen {
			t.Errorf("chunk %d length = %d, want %d", i, len(c), streamChunkLen)
		}
	}
	if last := chunks[len(chunks)-1]; last == "" || len(last) > streamChunkLen {
		t.Errorf("final chunk length = %d, want (0, %d]", len(last), streamChunkLen)
	}
	if got := joinStrings(chunks); got != res.Answer {
		t.Errorf("joined chunks lost text: %q vs %q", got, res.Answer)
	}
	if len(fake.prompts) != 0 {
		t.Errorf("router hit must not touch the LLM, saw %d prompts", len(fake.prompts))
	}
	if _, source, _, _ := lastAudit(t, st); source != store.SourceRouter {
		t.Errorf("audit source = %q", source)
	}
}

func TestRunStreamingLLMOffMiss(t *testing.T) {
	e, st := newEngine(t, nil) // AEGIS_LLM=off equivalent

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "tell me a story", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	want := "No deterministic rule matched and the LLM fallback is disabled (AEGIS_LLM=off)."
	if res.DecisionSource != store.SourceLLMOff {
		t.Fatalf("source = %q, want llm_disabled", res.DecisionSource)
	}
	if res.Answer != want {
		t.Errorf("answer = %q, want the kill-switch message", res.Answer)
	}
	if len(chunks) != 1 || chunks[0] != want {
		t.Errorf("chunks = %q, want the miss message emitted once", chunks)
	}
	if iface, source, outcome, _ := lastAudit(t, st); iface != store.IFaceCLI ||
		source != store.SourceLLMOff || outcome != "ok" {
		t.Errorf("audit = (%q, %q, %q)", iface, source, outcome)
	}
}

func TestRunStreamingLLMError(t *testing.T) {
	e, st := newEngine(t, &errLLM{err: errors.New("provider exploded")})

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "what is the meaning of life", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceError {
		t.Fatalf("source = %q, want error", res.DecisionSource)
	}
	if !strings.Contains(res.Answer, "LLM run failed:") || !strings.Contains(res.Answer, "provider exploded") {
		t.Errorf("answer = %q, want the wrapped failure text", res.Answer)
	}
	if len(chunks) < 2 || chunks[0] != "partial answer" {
		t.Errorf("chunks = %q, want the pre-failure delta then the failure notice", chunks)
	}
	if iface, source, outcome, _ := lastAudit(t, st); iface != store.IFaceCLI ||
		source != store.SourceError || outcome != "error" {
		t.Errorf("audit = (%q, %q, %q)", iface, source, outcome)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events`); n != 0 {
		t.Errorf("failed runs must not feed the mining corpus, rows = %d", n)
	}
}

func TestRunStreamingDeltaForwarding(t *testing.T) {
	llm := &streamLLM{
		deltas:  []string{"Hello ", "world"},
		tool:    "csv_stats",
		usage:   message.UsageDetails{InputTokenCount: 7, OutputTokenCount: 3},
		leadNil: true, // providers may emit payload-less keep-alive updates
	}
	e, st := newEngine(t, llm)

	var chunks []string
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "summarize the quarterly file", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceLLM || res.RuleID != "" {
		t.Fatalf("res = %+v", res)
	}
	if res.Answer != "Hello world" {
		t.Errorf("answer = %q, want the concatenated deltas", res.Answer)
	}
	if len(chunks) != 2 || chunks[0] != "Hello " || chunks[1] != "world" {
		t.Errorf("chunks = %q, want each delta forwarded verbatim", chunks)
	}
	if len(llm.prompts) != 1 || llm.prompts[0] != "summarize the quarterly file" {
		t.Errorf("prompts = %q", llm.prompts)
	}
	// The fallback row must carry the LLM's tool choice and summed token
	// counts — the miner's dominant-tool and cost signals.
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events
		WHERE tools_used='csv_stats' AND tokens_in=7 AND tokens_out=3
		AND raw_prompt='summarize the quarterly file'`); n != 1 {
		t.Errorf("fallback rows with tool choice + token counts = %d, want 1", n)
	}
}

func TestRunStreamingEmitError(t *testing.T) {
	e, st := newEngine(t, &streamLLM{deltas: []string{"a", "b", "c"}})

	calls := 0
	_, ctx := trace.New(context.Background(), "")
	res := e.RunStreaming(ctx, "tell me anything", func(string) error {
		calls++
		return errors.New("client hung up")
	})
	if res.DecisionSource != store.SourceError {
		t.Fatalf("source = %q, want error", res.DecisionSource)
	}
	if !strings.Contains(res.Answer, "client hung up") {
		t.Errorf("answer = %q, want the emit failure surfaced", res.Answer)
	}
	// Exactly two sink writes: the rejected delta, then the failure notice;
	// the delta loop must stop consuming the provider after the first error.
	if calls != 2 {
		t.Errorf("emit calls = %d, want 2 (delta attempt + failure notice)", calls)
	}
	if _, source, outcome, _ := lastAudit(t, st); source != store.SourceError || outcome != "error" {
		t.Errorf("audit = (%q, %q)", source, outcome)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events`); n != 0 {
		t.Errorf("aborted runs must not feed the mining corpus, rows = %d", n)
	}
}

func TestEmitChunks(t *testing.T) {
	var got []string
	emit := func(c string) error { got = append(got, c); return nil }

	emitChunks("", emit)
	if len(got) != 0 {
		t.Errorf("empty string emitted %d chunks, want 0", len(got))
	}

	// Exact multiple of the chunk size: two full slices, no empty tail chunk.
	got = nil
	exact := strings.Repeat("x", 2*streamChunkLen)
	emitChunks(exact, emit)
	if len(got) != 2 || got[0] != exact[:streamChunkLen] || got[1] != exact[streamChunkLen:] {
		t.Errorf("exact-multiple chunks = %q (len %d)", got, len(got))
	}

	// A remainder rides the final chunk.
	got = nil
	emitChunks(strings.Repeat("y", streamChunkLen+10), emit)
	if len(got) != 2 || len(got[1]) != 10 {
		t.Errorf("remainder chunks = %d, tail = %d bytes", len(got), len(got[len(got)-1]))
	}

	// A failing sink stops the slicing immediately: one attempt, no more.
	calls := 0
	emitChunks(strings.Repeat("z", 3*streamChunkLen), func(string) error {
		calls++
		return errors.New("sink closed")
	})
	if calls != 1 {
		t.Errorf("emit calls after sink error = %d, want 1", calls)
	}
}

func TestFinishRouterToolError(t *testing.T) {
	fake := &fakeLLM{}
	e, st := newEngine(t, fake)

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "/csv_head nonexistent.csv")
	// The rule matched but the tool failed. Both the audit row and the returned
	// Result carry the router source: a tool failure underneath a matched rule
	// is still a router decision (Hard Rule 6).
	if !strings.Contains(res.Answer, `command "csv_head" failed`) ||
		!strings.Contains(res.Answer, `path "nonexistent.csv" not found`) {
		t.Errorf("answer = %q, want the tool failure surfaced", res.Answer)
	}
	if res.DecisionSource != store.SourceRouter {
		t.Errorf("source = %q, want regex_router on the tool-error arm", res.DecisionSource)
	}
	if len(fake.prompts) != 0 {
		t.Errorf("a failed tool is still a router hit; LLM saw %d prompts", len(fake.prompts))
	}
	iface, source, outcome, rule := lastAudit(t, st)
	if iface != store.IFaceCLI || source != store.SourceRouter || outcome != "error" || rule != "csv_head" {
		t.Errorf("audit = (%q, %q, %q, %q)", iface, source, outcome, rule)
	}
}

func TestRunLLMErrorMiss(t *testing.T) {
	e, st := newEngine(t, &errLLM{err: errors.New("provider exploded")})

	_, ctx := trace.New(context.Background(), "")
	res := e.Run(ctx, "tell me a story")
	if res.DecisionSource != store.SourceError {
		t.Fatalf("source = %q, want error", res.DecisionSource)
	}
	if !strings.Contains(res.Answer, "LLM run failed:") || !strings.Contains(res.Answer, "provider exploded") {
		t.Errorf("answer = %q, want the wrapped failure text", res.Answer)
	}
	if _, source, outcome, _ := lastAudit(t, st); source != store.SourceError || outcome != "error" {
		t.Errorf("audit = (%q, %q)", source, outcome)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events`); n != 0 {
		t.Errorf("failed runs must not feed the mining corpus, rows = %d", n)
	}
}

func TestAuditWithoutStoreSkipsWrite(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.Store = nil // a store-less engine must still route without panicking
	_, ctx := trace.New(context.Background(), "")
	if res := e.Run(ctx, "/hostname"); res.DecisionSource != store.SourceRouter || res.Answer == "" {
		t.Fatalf("res = %+v", res)
	}
}

func TestToolNamesAndTokenUsageEdgeInputs(t *testing.T) {
	if names := ToolNames(nil); names != nil {
		t.Errorf("ToolNames(nil) = %v, want nil", names)
	}
	if in, out := tokenUsage(nil); in != 0 || out != 0 {
		t.Errorf("tokenUsage(nil) = (%d, %d), want (0, 0)", in, out)
	}
	// Usage contents sum across messages; tool calls are collected by name.
	resp := &agent.Response{Messages: []*message.Message{
		{Contents: message.Contents{&message.UsageContent{
			Details: message.UsageDetails{InputTokenCount: 3, OutputTokenCount: 4}}}},
		{Contents: message.Contents{
			&message.TextContent{Text: "done"},
			&message.FunctionCallContent{Name: "read_csv", CallID: "c1"},
			&message.UsageContent{Details: message.UsageDetails{InputTokenCount: 5}},
		}},
	}}
	if names := ToolNames(resp); len(names) != 1 || names[0] != "read_csv" {
		t.Errorf("ToolNames = %v, want [read_csv]", names)
	}
	if in, out := tokenUsage(resp); in != 8 || out != 4 {
		t.Errorf("tokenUsage = (%d, %d), want summed counts (8, 4)", in, out)
	}
}

// fakeClassifier resolves every miss with a canned payload (or declines),
// counting calls so tests can prove when the tier runs and when it must not.
type fakeClassifier struct {
	calls  int
	answer string
	tool   string
	ok     bool
}

func (f *fakeClassifier) Classify(_ context.Context, _ string) (string, string, bool) {
	f.calls++
	return f.answer, f.tool, f.ok
}

// TestClassifierResolvesMiss: a miss the classifier resolves answers with
// decision_source=llm_classifier, writes the audit row with the fast-tier
// model, and seeds the mining corpus with the tool choice.
func TestClassifierResolvesMiss(t *testing.T) {
	e, st := newEngine(t, &fakeLLM{})
	fc := &fakeClassifier{answer: "classified answer", tool: "read_csv", ok: true}
	e.Classifier, e.ClassifierModel = fc, "fast-model"
	_, ctx := trace.New(context.Background(), "")

	res := e.Run(ctx, "please show me the sample orders file")
	if res.DecisionSource != store.SourceLLMClassifier || res.Answer != "classified answer" {
		t.Fatalf("res = %+v, want llm_classifier with the classified answer", res)
	}
	if fc.calls != 1 {
		t.Errorf("classifier calls = %d, want 1", fc.calls)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_events
		WHERE decision_source='llm_classifier' AND model='fast-model'`); n != 1 {
		t.Errorf("llm_classifier audit rows = %d, want 1", n)
	}
	// The corpus row is what makes patterns graduate into regex rules.
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events
		WHERE tools_used='read_csv' AND model='fast-model'`); n != 1 {
		t.Errorf("classifier corpus rows = %d, want 1", n)
	}
}

// TestClassifierDeclineFallsThrough: a declining classifier is invisible —
// the smart LLM answers with decision_source=llm.
func TestClassifierDeclineFallsThrough(t *testing.T) {
	e, st := newEngine(t, &fakeLLM{})
	fc := &fakeClassifier{ok: false}
	e.Classifier, e.ClassifierModel = fc, "fast-model"
	_, ctx := trace.New(context.Background(), "")

	res := e.Run(ctx, "what is the meaning of life?")
	if res.DecisionSource != store.SourceLLM || res.Answer != "llm answer" {
		t.Fatalf("res = %+v, want the plain LLM fallback", res)
	}
	if fc.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 (it ran and declined)", fc.calls)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM audit_events
		WHERE decision_source='llm_classifier'`); n != 0 {
		t.Errorf("declined classification left audit rows: %d", n)
	}
}

// TestClassifierSkippedWhenLLMOff: the kill switch is absolute — no
// provider, no classifier, fast llm_disabled answer.
func TestClassifierSkippedWhenLLMOff(t *testing.T) {
	e, _ := newEngine(t, nil)
	fc := &fakeClassifier{answer: "x", tool: "read_csv", ok: true}
	e.Classifier, e.ClassifierModel = fc, "fast-model"
	_, ctx := trace.New(context.Background(), "")

	res := e.Run(ctx, "anything at all")
	if res.DecisionSource != store.SourceLLMOff {
		t.Fatalf("res = %+v, want llm_disabled", res)
	}
	if fc.calls != 0 {
		t.Errorf("classifier ran %d times under AEGIS_LLM=off", fc.calls)
	}
}

// TestClassifierSkippedOnRouterHit: deterministic hits never pay even the
// fast tier.
func TestClassifierSkippedOnRouterHit(t *testing.T) {
	e, _ := newEngine(t, &fakeLLM{})
	fc := &fakeClassifier{answer: "x", tool: "read_csv", ok: true}
	e.Classifier, e.ClassifierModel = fc, "fast-model"
	_, ctx := trace.New(context.Background(), "")

	res := e.Run(ctx, "/uptime")
	if res.DecisionSource != store.SourceRouter {
		t.Fatalf("res = %+v, want regex_router", res)
	}
	if fc.calls != 0 {
		t.Errorf("classifier ran on a router hit (%d calls)", fc.calls)
	}
}

// TestClassifierStreamingEmits: the streaming path chunk-classifies the
// same way, through the same SSE contract.
func TestClassifierStreamingEmits(t *testing.T) {
	e, st := newEngine(t, &fakeLLM{})
	fc := &fakeClassifier{answer: "streamed classified answer", tool: "csv_stats", ok: true}
	e.Classifier, e.ClassifierModel = fc, "fast-model"
	_, ctx := trace.New(context.Background(), "")

	var chunks []string
	res := e.RunStreaming(ctx, "summarize the sample file", func(c string) error {
		chunks = append(chunks, c)
		return nil
	})
	if res.DecisionSource != store.SourceLLMClassifier {
		t.Fatalf("res = %+v, want llm_classifier", res)
	}
	if len(chunks) == 0 || strings.Join(chunks, "") != "streamed classified answer" {
		t.Errorf("chunks = %q, want the classified answer reassembled", chunks)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM fallback_events
		WHERE tools_used='csv_stats'`); n != 1 {
		t.Errorf("classifier corpus rows = %d, want 1", n)
	}
}

// TestRunStreamingShadowExecutesToolOnce pins the streaming shadow
// contract: RunStreaming routes (executing the matched tool once) and must
// never re-route internally. The old implementation delegated shadow rules
// to Run, which routed AGAIN — double side effects for tools like make_dir
// or download when reached through ?stream=1.
func TestRunStreamingShadowExecutesToolOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		llm  LLMRunner
	}{
		{"llm on", &fakeLLM{}},
		{"llm off", nil}, // shadow degrades to the rule's answer
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ct, err := tools.New(tools.Config{
				Name: "count_me", Description: "increments a counter for the test",
			}, func(_ context.Context, _ struct{}) (string, error) {
				calls.Add(1)
				return "counted", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			reg, err := tools.NewRegistry(ct)
			if err != nil {
				t.Fatal(err)
			}
			r, err := router.New(reg, []router.RuleDef{{
				Name: "count_me", Pattern: "count me", Tool: "count_me",
				ArgsTemplate: "{}", Origin: "mined", State: router.RuleShadow,
			}})
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			e := &Engine{Router: r, LLM: tc.llm, Store: st, IFace: store.IFaceCLI}

			_, ctx := trace.New(context.Background(), "")
			res := e.RunStreaming(ctx, "count me", func(string) error { return nil })

			if got := calls.Load(); got != 1 {
				t.Errorf("tool executed %d times, want exactly 1", got)
			}
			if res.DecisionSource == "" {
				t.Error("decision_source empty — every run reports one (Hard Rule 6)")
			}
		})
	}
}
