package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAnalyzeLog(t *testing.T) (Tool, string) {
	t.Helper()
	ws := t.TempDir()
	tl, err := NewAnalyzeLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	return tl, ws
}

func TestAnalyzeLogPerfSummary(t *testing.T) {
	tl, ws := newAnalyzeLog(t)
	p := filepath.Join(ws, "app.log")
	os.WriteFile(p, []byte("GET /a status=200 latency=100ms\nGET /b status=500 latency=2000ms\n"), 0o644)
	out, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"app.log","kind":"perf"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if want := "2 requests"; !strings.Contains(string(b), want) {
		t.Fatalf("out=%s", b)
	}
}

func TestAnalyzeLogContainment(t *testing.T) {
	tl, _ := newAnalyzeLog(t)
	// escape attempt
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"../etc/passwd","kind":"perf"}`)); err == nil {
		t.Fatal("escape accepted — containment broken")
	}
	// absolute outside workspace
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"/etc/hosts","kind":"perf"}`)); err == nil {
		t.Fatal("absolute path accepted")
	}
}

func TestAnalyzeLogKindsAndValidation(t *testing.T) {
	tl, ws := newAnalyzeLog(t)
	p := filepath.Join(ws, "e.log")
	os.WriteFile(p, []byte("2026-09-28 09:00 cmd=/x\nERROR db timeout after 5000ms\n10.0.0.1 GET /a 200 10ms\n"), 0o644)
	for _, kind := range []string{"exceptions", "access", "behaviour"} {
		out, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"e.log","kind":"`+kind+`"}`))
		if err != nil {
			t.Fatalf("kind %s: %v", kind, err)
		}
		b, _ := json.Marshal(out)
		if !strings.Contains(string(b), `"kind":"`+kind+`"`) {
			t.Fatalf("kind echo missing: %s", b)
		}
	}
	// bad kind
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"e.log","kind":"nope"}`)); err == nil {
		t.Fatal("bad kind accepted")
	}
	// missing path
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"kind":"perf"}`)); err == nil {
		t.Fatal("missing path accepted")
	}
	// missing file
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"nope.log"}`)); err == nil {
		t.Fatal("missing file accepted")
	}
	// max_lines clamp path (20001 → 20000) just executes fine
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":"e.log","kind":"perf","max_lines":20001}`)); err != nil {
		t.Fatalf("clamp: %v", err)
	}
}
