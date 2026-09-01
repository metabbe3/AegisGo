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
