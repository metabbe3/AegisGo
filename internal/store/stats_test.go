package store

import (
	"context"
	"reflect"
	"testing"
)

func TestStatsEmpty(t *testing.T) {
	s := openMem(t)
	snap, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalRuns != 0 || snap.DeflectionRate != 0 {
		t.Errorf("totals = %d runs / rate %v, want 0/0", snap.TotalRuns, snap.DeflectionRate)
	}
	if len(snap.BySource) != 0 || len(snap.ByInterface) != 0 ||
		len(snap.AvgLatencyMS) != 0 || len(snap.RulesByState) != 0 {
		t.Errorf("maps not empty: %+v", snap)
	}
	if len(snap.TopFallbacks) != 0 {
		t.Errorf("TopFallbacks = %v, want empty", snap.TopFallbacks)
	}
}

func TestStatsPopulated(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	// Six runs: 3 router (lat 3,4,5 -> avg 4), 2 llm (lat 100,105 ->
	// avg 102.5, truncated to 102), 1 error (lat 7). Router deflection
	// is 3/6 = 0.5.
	audits := []AuditEvent{
		{TraceID: "t1", Interface: IFaceCLI, DecisionSource: SourceRouter, RuleID: "uptime", Prompt: "/uptime", LatencyMS: 3},
		{TraceID: "t2", Interface: IFaceREST, DecisionSource: SourceRouter, RuleID: "uptime", Prompt: "/uptime", LatencyMS: 4},
		{TraceID: "t3", Interface: IFaceCLI, DecisionSource: SourceRouter, RuleID: "uptime", Prompt: "/uptime", LatencyMS: 5},
		{TraceID: "t4", Interface: IFaceREST, DecisionSource: SourceLLM, Model: "llama3", Prompt: "what changed", LatencyMS: 100, TokensIn: 10, TokensOut: 20},
		{TraceID: "t5", Interface: IFaceREST, DecisionSource: SourceLLM, Model: "llama3", Prompt: "what else", LatencyMS: 105, TokensIn: 8, TokensOut: 12},
		{TraceID: "t6", Interface: IFaceCLI, DecisionSource: SourceError, Prompt: "", LatencyMS: 7, Outcome: "error"},
	}
	for _, ev := range audits {
		s.Audit(ctx, ev)
	}

	// Two fallback shapes: one seen 3 times, one once — the mining preview
	// must order them by count descending.
	fallbacks := []FallbackEvent{
		{TraceID: "f1", NormalizedPrompt: "summarize <path>", RawPrompt: "summarize data/2024.csv", Model: "llama3"},
		{TraceID: "f2", NormalizedPrompt: "summarize <path>", RawPrompt: "summarize data/2025.csv"},
		{TraceID: "f3", NormalizedPrompt: "summarize <path>", RawPrompt: "summarize data/2026.csv"},
		{TraceID: "f4", NormalizedPrompt: "count rows in <q>", RawPrompt: `count rows in "sales.csv"`},
	}
	for _, ev := range fallbacks {
		s.RecordFallback(ev)
	}

	insertRule(t, s, "uptime", "seed", RuleStateActive, 1)
	insertRule(t, s, "echo_shape", "mined", RuleStateShadow, 1)
	insertRule(t, s, "old_rule", "mined", RuleStateDemoted, 0)
	mustFlush(t, s)

	snap, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if snap.TotalRuns != 6 {
		t.Errorf("TotalRuns = %d, want 6", snap.TotalRuns)
	}
	wantSource := map[string]int{SourceRouter: 3, SourceLLM: 2, SourceError: 1}
	if !reflect.DeepEqual(snap.BySource, wantSource) {
		t.Errorf("BySource = %v, want %v", snap.BySource, wantSource)
	}
	wantAvg := map[string]int64{SourceRouter: 4, SourceLLM: 102, SourceError: 7}
	if !reflect.DeepEqual(snap.AvgLatencyMS, wantAvg) {
		t.Errorf("AvgLatencyMS = %v, want %v (102.5 truncates to 102)", snap.AvgLatencyMS, wantAvg)
	}
	wantIface := map[string]int{IFaceCLI: 3, IFaceREST: 3}
	if !reflect.DeepEqual(snap.ByInterface, wantIface) {
		t.Errorf("ByInterface = %v, want %v", snap.ByInterface, wantIface)
	}
	wantRules := map[string]int{RuleStateActive: 1, RuleStateShadow: 1, RuleStateDemoted: 1}
	if !reflect.DeepEqual(snap.RulesByState, wantRules) {
		t.Errorf("RulesByState = %v, want %v", snap.RulesByState, wantRules)
	}
	if snap.DeflectionRate != 0.5 {
		t.Errorf("DeflectionRate = %v, want 0.5 (3 router runs of 6)", snap.DeflectionRate)
	}
	wantTop := []ShapeCount{
		{Shape: "summarize <path>", Count: 3},
		{Shape: "count rows in <q>", Count: 1},
	}
	if !reflect.DeepEqual(snap.TopFallbacks, wantTop) {
		t.Errorf("TopFallbacks = %v, want %v (descending by count)", snap.TopFallbacks, wantTop)
	}
}

func TestReplayNullColumns(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	s.Audit(ctx, AuditEvent{
		TraceID: "tr", Interface: IFaceREST, DecisionSource: SourceRouter,
		RuleID: "uptime", Prompt: "/uptime", LatencyMS: 3,
	})
	s.Audit(ctx, AuditEvent{
		TraceID: "tr", Interface: IFaceREST, DecisionSource: SourceLLM,
		Model: "llama3", Prompt: "hello", LatencyMS: 900, TokensIn: 1, TokensOut: 2,
	})
	mustFlush(t, s)

	trail, err := s.Replay(ctx, "tr")
	if err != nil {
		t.Fatal(err)
	}
	if len(trail) != 2 {
		t.Fatalf("trail rows = %d, want 2", len(trail))
	}
	router, llm := trail[0], trail[1]
	if router.RuleID != "uptime" || router.Model != "" {
		t.Errorf("router row = %+v, want rule=uptime and empty model", router)
	}
	// NULL rule_id and NULL model must scan as "" — the pointer branch.
	if llm.RuleID != "" || llm.Model != "llama3" {
		t.Errorf("llm row = %+v, want empty rule and model=llama3", llm)
	}
	if llm.TS == "" || llm.Interface != IFaceREST || llm.DecisionSource != SourceLLM {
		t.Errorf("llm row header fields = %+v", llm)
	}
	if llm.LatencyMS != 900 || llm.Outcome != "ok" {
		t.Errorf("llm row = %+v, want latency 900 and default outcome ok", llm)
	}
}

func TestReplayEmptyTrace(t *testing.T) {
	s := openMem(t)
	trail, err := s.Replay(context.Background(), "no-such-trace")
	if err != nil {
		t.Fatalf("Replay(unknown trace): %v", err)
	}
	if len(trail) != 0 {
		t.Errorf("trail = %d rows, want 0", len(trail))
	}
}
