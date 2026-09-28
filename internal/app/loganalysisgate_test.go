package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/loganalysis"
	"aegisgo/internal/store"
)

func newLaGate(t *testing.T) (*LogAnalysisGate, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &LogAnalysisGate{
		QueryAuditRows: func(ctx context.Context, n int) ([]map[string]any, error) {
			return st.QueryMaps(ctx,
				`SELECT interface, decision_source, rule_id, outcome, latency_ms, confidence, tokens_in, tokens_out
				 FROM audit_events ORDER BY rowid DESC LIMIT ?`, n)
		},
	}, st
}

func writeLog(t *testing.T, lines string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAPIPerfCommand(t *testing.T) {
	g, _ := newLaGate(t)
	p := writeLog(t, strings.Join([]string{
		`2026-09-28 09:00:01 GET /v1/answers/abc123 status=200 latency=120ms`,
		`2026-09-28 09:00:02 GET /v1/answers/def456 status=200 latency=80ms`,
		`2026-09-28 09:00:03 POST /v1/agent/run status=500 latency=2500ms`,
		`2026-09-28 09:00:04 GET /v1/stats status=200 latency=95ms`,
	}, "\n"))
	out := g.HandleText(context.Background(), "/api_perf "+p+" -n 100")
	for _, want := range []string{"API performance", "p50", "p95", "/v1/answers/:id", "500"} {
		if !strings.Contains(out, want) {
			t.Errorf("api_perf missing %q:\n%s", want, out)
		}
	}
	// usage + missing file
	if out := g.HandleText(context.Background(), "/api_perf"); !strings.Contains(out, "usage") {
		t.Errorf("usage = %q", out)
	}
	if out := g.HandleText(context.Background(), "/api_perf /nope-xyz"); !strings.Contains(out, "⚠️") {
		t.Errorf("missing = %q", out)
	}
}

func TestExceptionsCommand(t *testing.T) {
	g, _ := newLaGate(t)
	p := writeLog(t, strings.Join([]string{
		`INFO ok`,
		`ERROR 2026-09-28 db timeout after 5000ms query="SELECT 1"`,
		`ERROR 2026-09-28 db timeout after 6000ms query="SELECT 2"`,
		`panic: runtime error: index out of range [5]`,
	}, "\n"))
	out := g.HandleText(context.Background(), "/exceptions "+p)
	for _, want := range []string{"Exceptions", "DUR", "2×"} {
		if !strings.Contains(out, want) {
			t.Errorf("exceptions missing %q:\n%s", want, out)
		}
	}
}

func TestAccessCommand(t *testing.T) {
	g, _ := newLaGate(t)
	p := writeLog(t, strings.Join([]string{
		`10.1.2.3 - - [28/Sep/2026:09:00:01] "GET /v1/stats HTTP/1.1" 200 120ms "Mozilla/5.0"`,
		`10.1.2.3 - - [28/Sep/2026:09:00:02] "GET /v1/stats HTTP/1.1" 200 80ms "Mozilla/5.0"`,
		`10.9.9.9 - - [28/Sep/2026:09:00:03] "POST /v1/agent/run HTTP/1.1" 500 2500ms "curl/8.0"`,
	}, "\n"))
	out := g.HandleText(context.Background(), "/access "+p)
	for _, want := range []string{"Access", "10.1.2.3", "/v1/stats", "200"} {
		if !strings.Contains(out, want) {
			t.Errorf("access missing %q:\n%s", want, out)
		}
	}
}

func TestAuditCommand(t *testing.T) {
	g, st := newLaGate(t)
	ctx := context.Background()
	// seed audit rows through the real store path
	for i := 0; i < 3; i++ {
		st.Audit(ctx, store.AuditEvent{
			TraceID: "t1", Interface: "telegram", DecisionSource: "regex_router",
			RuleID: "uptime", Outcome: "ok", LatencyMS: int64(10 + i), Confidence: 90,
		})
	}
	st.Audit(ctx, store.AuditEvent{
		TraceID: "t2", Interface: "http", DecisionSource: "llm",
		Outcome: "error", LatencyMS: 800, Confidence: 40,
	})
	// batcher is async — wait briefly for rows
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		st.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n)
		if n == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out := g.HandleText(ctx, "/audit")
	for _, want := range []string{"Audit trail", "regex_router", "llm", "conf avg"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q:\n%s", want, out)
		}
	}
}

func TestBehaviourCommand(t *testing.T) {
	g, _ := newLaGate(t)
	p := writeLog(t, strings.Join([]string{
		`2026-09-28 09:00 cmd=/uptime user=1`,
		`2026-09-28 09:01 cmd=/uptime user=1`,
		`2026-09-28 09:01 cmd=/disk user=2`,
		`2026-09-28 22:05 cmd=/watchlist user=1`,
	}, "\n"))
	out := g.HandleText(context.Background(), "/behaviour "+p+" -n 100")
	for _, want := range []string{"Behaviour", "/uptime", "Peak hours"} {
		if !strings.Contains(out, want) {
			t.Errorf("behaviour missing %q:\n%s", want, out)
		}
	}
}

func TestPerfEmojiBands(t *testing.T) {
	if perfEmoji(0.5, 500) != "🟢" || perfEmoji(2, 500) != "🟡" || perfEmoji(0.5, 1500) != "🟡" || perfEmoji(6, 500) != "🔴" || perfEmoji(0.5, 4000) != "🔴" {
		t.Fatal("perfEmoji bands wrong")
	}
}

func TestAnalyzeViaToolDelegation(t *testing.T) {
	// ExecTool wired → commands delegate to the analyze_log tool
	// (containment lives there).
	g := &LogAnalysisGate{
		ExecTool: func(ctx context.Context, name string, args map[string]any) (any, error) {
			if name != "analyze_log" {
				t.Fatalf("tool = %s", name)
			}
			if args["kind"] != "perf" || args["path"] != "/x.log" {
				t.Fatalf("args = %v", args)
			}
			return map[string]any{"kind": "perf", "path": "/x.log", "lines_read": 2,
				"summary": "2 requests, p50 10ms"}, nil
		},
	}
	out := g.HandleText(context.Background(), "/api_perf /x.log -n 100")
	if !strings.Contains(out, "2 requests") || !strings.Contains(out, "p50 10ms") {
		t.Fatalf("delegated = %q", out)
	}
	// tool error → warning surfaced, command short-circuits
	g2 := &LogAnalysisGate{
		ExecTool: func(ctx context.Context, name string, args map[string]any) (any, error) {
			return nil, fmt.Errorf("path %q escapes the workspace", args["path"])
		},
	}
	out2 := g2.HandleText(context.Background(), "/api_perf ../etc/passwd")
	if !strings.Contains(out2, "⚠️") || !strings.Contains(out2, "escapes") {
		t.Fatalf("escape = %q", out2)
	}
}

func TestSummarizeToolOutputShapes(t *testing.T) {
	if s := summarizeToolOutput(map[string]any{"summary": "abc"}); s != "abc" {
		t.Fatalf("map = %q", s)
	}
	// struct without map: json round-trip path
	type flat struct {
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	}
	if s := summarizeToolOutput(flat{Kind: "perf", Summary: "xyz"}); s != "xyz" {
		t.Fatalf("struct = %q", s)
	}
}

func TestPathAndNBoundaries(t *testing.T) {
	// defaults, -n valid, -n out of range, missing path, junk tokens
	if p, n, err := pathAndN("x.log", 500); err != nil || p != "x.log" || n != 500 {
		t.Fatalf("plain: %v %v %v", p, n, err)
	}
	if p, n, err := pathAndN("x.log -n 7", 500); err != nil || p != "x.log" || n != 7 {
		t.Fatalf("-n: %v %v %v", p, n, err)
	}
	if _, _, err := pathAndN("x.log -n 0", 500); err == nil {
		t.Fatal("n=0 accepted")
	}
	if _, _, err := pathAndN("x.log -n 99999", 500); err == nil {
		t.Fatal("n>20000 accepted")
	}
	if _, _, err := pathAndN("-n 5", 500); err == nil {
		t.Fatal("missing path accepted")
	}
	if p, _, err := pathAndN("a.log b.log -n 5", 500); err != nil || p != "a.log" {
		t.Fatalf("extra token: %v %v", p, err)
	}
}

func TestKvBlockEmpty(t *testing.T) {
	if kvBlock("X", nil) != "" {
		t.Fatal("empty kvBlock")
	}
	if s := kvBlock("X", []loganalysis.KV{{Key: "k", Count: 2}}); !strings.Contains(s, "2×") || !strings.Contains(s, "*X*") {
		t.Fatalf("kvBlock=%q", s)
	}
}
