package loganalysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Package-level tests pin the pure computations the app layer renders:
// perf percentiles, route normalization, template grouping, audit rollup,
// behaviour histograms. (The Telegram commands are covered in
// internal/app; here we test the math itself.)
func writeFile(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnalyzeAPIPerfMath(t *testing.T) {
	p := writeFile(t, "GET /a status=200 latency=100ms\n"+
		"GET /a status=200 latency=300ms\n"+
		"GET /b status=500 latency=2000ms\n")
	r, err := AnalyzeAPIPerf(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Requests != 3 || r.Errors != 1 {
		t.Fatalf("reqs=%d errs=%d", r.Requests, r.Errors)
	}
	if r.P50 != 300 { // sorted [100,300,2000] → idx(0.5*2)=1
		t.Fatalf("p50=%v", r.P50)
	}
	if r.Slow != 1 {
		t.Fatalf("slow=%d", r.Slow)
	}
	if r.ErrPct < 33.3 || r.ErrPct > 33.4 {
		t.Fatalf("errpct=%v", r.ErrPct)
	}
}

func TestNormalizeRouteIds(t *testing.T) {
	cases := map[string]string{
		"/v1/answers/abc123":     "/v1/answers/:id",
		"/v1/answers/123":        "/v1/answers/:id",
		"/v1/answers/deadbeef42": "/v1/answers/:id",
		"/v1/stats":              "/v1/stats",
		"/v1/answers/xyz?u=1":    "/v1/answers/xyz", // plain word stays
		"/healthz":               "/healthz",
	}
	for in, want := range cases {
		if got := normalizeRoute(in); got != want {
			t.Errorf("normalizeRoute(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseLatMSVariants(t *testing.T) {
	cases := map[string]float64{
		"took 120ms":   120,
		"took 1.5s":    1500,
		"latency=42ms": 42,
		"5 status ok":  -1, // bare number ≠ duration
		"nothing":      -1,
	}
	for in, want := range cases {
		if got := parseLatMS(in); got != want {
			t.Errorf("parseLatMS(%q)=%v want %v", in, got, want)
		}
	}
}

func TestAnalyzeExceptionsGroups(t *testing.T) {
	p := writeFile(t, "INFO ok\n"+
		"ERROR db timeout after 5000ms\n"+
		"ERROR db timeout after 6000ms\n"+
		"panic: index out of range [5]\n")
	r, err := AnalyzeExceptions(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.ErrorLines != 3 {
		t.Fatalf("errlines=%d", r.ErrorLines)
	}
	if len(r.Top) < 2 {
		t.Fatalf("templates=%v", r.Top)
	}
	if r.Top[0].Count != 2 || r.Top[0].Key == "" {
		t.Fatalf("top=%v", r.Top[0])
	}
}

func TestAnalyzeAuditRowsRollup(t *testing.T) {
	rows := []map[string]any{
		{"decision_source": "regex_router", "rule_id": "uptime", "outcome": "ok", "latency_ms": int64(10), "confidence": int64(100)},
		{"decision_source": "regex_router", "rule_id": "uptime", "outcome": "ok", "latency_ms": int64(30), "confidence": int64(90)},
		{"decision_source": "llm", "rule_id": nil, "outcome": "error", "latency_ms": int64(900), "confidence": int64(40)},
	}
	s := AnalyzeAuditRows(rows)
	if s.Total != 3 || s.ErrRows != 1 {
		t.Fatalf("total=%d err=%d", s.Total, s.ErrRows)
	}
	if s.ConfLow != 1 {
		t.Fatalf("conflow=%d", s.ConfLow)
	}
	if s.P50 != 30 { // [10,30,900] idx 1
		t.Fatalf("p50=%v", s.P50)
	}
	if s.BySource[0].Key != "regex_router" || s.BySource[0].Count != 2 {
		t.Fatalf("src=%v", s.BySource)
	}
}

func TestAnalyzeBehaviourHistogram(t *testing.T) {
	p := writeFile(t, "2026-09-28 09:00 cmd=/uptime\n"+
		"2026-09-28 09:01 cmd=/uptime\n"+
		"2026-09-28 22:05 cmd=/disk\n")
	r, err := AnalyzeBehaviour(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Actions != 3 {
		t.Fatalf("actions=%d", r.Actions)
	}
	if r.TopCmds[0].Key != "/uptime" || r.TopCmds[0].Count != 2 {
		t.Fatalf("cmds=%v", r.TopCmds)
	}
	if len(r.HourHist) != 2 {
		t.Fatalf("hours=%v", r.HourHist)
	}
}

func TestReadLastNTailWindow(t *testing.T) {
	var s string
	for i := 0; i < 30; i++ {
		s += "line\n"
	}
	p := writeFile(t, s)
	lines, err := readLastN(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 {
		t.Fatalf("n=%d", len(lines))
	}
}

func TestNormalizeMasksEverything(t *testing.T) {
	in := `2026-09-28 09:00:01 trace_id=3f2b1c4d-aaaa-bbbb-cccc-dddddddddddd ip=10.1.2.3 took 187ms "SELECT x" [worker-7] port 8080`
	out := Normalize(in)
	for _, want := range []string{"trace_id=N", "IP", "DUR", `"STR"`, "[...]", "port N"} {
		if !strings.Contains(out, want) {
			t.Errorf("Normalize missing %q: %q", want, out)
		}
	}
}

func TestFnumAndPct(t *testing.T) {
	if fnum(nil) != -1 || fnum("x") != -1 {
		t.Fatal("fnum invalid")
	}
	if pct(1, 0) != 0 || pct(1, 2) != 50 {
		t.Fatal("pct")
	}
}

func TestAnalyzeAccessFallbackStatus(t *testing.T) {
	// nginx-ish tanpa "HTTP/1.1" marker: status polos setelah path
	p := writeFile(t, `10.0.0.9 GET /a 404 12ms\n10.0.0.9 GET /a 404 15ms\n`)
	r, err := AnalyzeAccess(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TopStatus) == 0 || r.TopStatus[0].Key != "404" {
		t.Fatalf("status=%v", r.TopStatus)
	}
}
