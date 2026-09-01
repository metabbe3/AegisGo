package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp/syntax"
	"strings"
	"testing"

	"aegisgo/internal/tools"
)

func testRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	wd, err := os.Getwd()
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
	return reg
}

func TestRouterSeededMatches(t *testing.T) {
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}

	d := r.Handle(context.Background(), "/uptime")
	if !d.Handled || d.RuleID != "uptime" || d.Err != nil {
		t.Fatalf("uptime: %+v", d)
	}
	var out struct {
		Command string `json:"command"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(d.Text()), &out); err != nil {
		t.Fatalf("text not json: %v (%s)", err, d.Text())
	}
	if out.Command != "uptime" || out.Output == "" {
		t.Errorf("uptime output = %+v", out)
	}
}

func TestRouterCaptureArgs(t *testing.T) {
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}

	// specific rule wins: explicit row count splices into max_rows
	d := r.Handle(context.Background(), "/csv_head testdata/sample.csv 3")
	if !d.Handled || d.RuleID != "csv_head_n" || d.Err != nil {
		t.Fatalf("csv_head_n: %+v", d)
	}
	var out struct {
		Rows [][]string `json:"rows"`
	}
	if err := json.Unmarshal([]byte(d.Text()), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 3 {
		t.Errorf("rows = %d, want 3 (args splice broken)", len(out.Rows))
	}

	// general rule: no count → default preview
	d = r.Handle(context.Background(), "/csv_summary testdata/sample.csv")
	if !d.Handled || d.RuleID != "csv_summary" || d.Err != nil {
		t.Fatalf("csv_summary: %+v", d)
	}
	var stats struct {
		TotalRows int `json:"total_rows"`
	}
	if err := json.Unmarshal([]byte(d.Text()), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.TotalRows != 10 {
		t.Errorf("total_rows = %d, want 10", stats.TotalRows)
	}
}

func TestRouterMissAndEscapes(t *testing.T) {
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Handle(context.Background(), "what is the meaning of life?"); d.Handled {
		t.Errorf("free-form prompt matched rule %q", d.RuleID)
	}

	// Captures carry attacker text into args — the tool must reject paths
	// escaping the workspace (path sandbox still applies on the router path).
	d := r.Handle(context.Background(), "/csv_summary ../../etc/passwd")
	if !d.Handled || d.Err == nil {
		t.Errorf("escape attempt should fail in the tool: %+v", d)
	}

	// Subcommand smuggling does not match any rule.
	if d := r.Handle(context.Background(), "/uptime; rm -rf /"); d.Handled {
		t.Errorf("smuggled suffix matched rule %q (anchor broken)", d.RuleID)
	}
}

func TestRouterRuleValidation(t *testing.T) {
	reg := testRegistry(t)
	if _, err := New(reg, []RuleDef{{Name: "x", Pattern: "(", Tool: "read_csv"}}); err == nil {
		t.Error("bad regex accepted")
	}
	if _, err := New(reg, []RuleDef{{Name: "x", Pattern: "a", Tool: "nope"}}); err == nil {
		t.Error("unknown tool accepted")
	}
	if _, err := New(reg, []RuleDef{
		{Name: "x", Pattern: "a", Tool: "read_csv"},
		{Name: "x", Pattern: "b", Tool: "read_csv"},
	}); err == nil {
		t.Error("duplicate rule accepted")
	}
}

func TestRouterSwap(t *testing.T) {
	reg := testRegistry(t)
	r, err := New(reg, Seeded())
	if err != nil {
		t.Fatal(err)
	}
	// Swap to a single rule; uptime no longer routes.
	if err := r.Swap([]RuleDef{{Name: "only", Pattern: "/only", Tool: "read_csv", ArgsTemplate: `{"path":"testdata/sample.csv"}`}}); err != nil {
		t.Fatal(err)
	}
	if d := r.Handle(context.Background(), "/uptime"); d.Handled {
		t.Error("swapped-out rule still matched")
	}
	if d := r.Handle(context.Background(), "/only"); !d.Handled || d.RuleID != "only" {
		t.Errorf("new rule: %+v", d)
	}
}

func TestCompileAnchorsPattern(t *testing.T) {
	c, err := RuleDef{Name: "uptime", Pattern: `/uptime`, Tool: "system_command"}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Regexp().String(); !strings.Contains(got, "^(?:") || !strings.HasSuffix(got, ")$") {
		t.Errorf("anchored pattern = %q, want ^(?:…)$ wrapping", got)
	}
	if !c.Regexp().MatchString("/uptime") {
		t.Error("exact prompt must match the anchored pattern")
	}
	if c.Regexp().MatchString("say /uptime now") {
		t.Error("substring-containing prompt must not match (anchor broken)")
	}
}

func TestCompileErrors(t *testing.T) {
	_, err := RuleDef{Name: "bad", Pattern: "(", Tool: "read_csv"}.Compile()
	if err == nil {
		t.Fatal("invalid regex accepted")
	}
	if !strings.Contains(err.Error(), "rule bad:") {
		t.Errorf("error must name the rule: %v", err)
	}
	var rerr *syntax.Error
	if !errors.As(err, &rerr) {
		t.Errorf("error must wrap the underlying regexp failure: %v", err)
	}

	_, err = RuleDef{Name: "toolless", Pattern: "a"}.Compile()
	if err == nil {
		t.Fatal("missing tool accepted")
	}
	if !strings.Contains(err.Error(), "tool is required") {
		t.Errorf("error = %v, want 'tool is required'", err)
	}
	if !strings.Contains(err.Error(), "toolless") {
		t.Errorf("error must name the rule: %v", err)
	}

	// Swap validates identically (the hot-reload entry point): a bad set is
	// rejected atomically and the previous rules keep serving.
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Swap([]RuleDef{{Name: "s", Pattern: "a", Tool: "ghost_tool"}}); err == nil {
		t.Error("Swap accepted an unknown tool")
	}
	if d := r.Handle(context.Background(), "/uptime"); !d.Handled || d.RuleID != "uptime" {
		t.Errorf("rejected Swap must keep previous rules: %+v", d)
	}
}

func TestSampleForTestForcesSampling(t *testing.T) {
	trueFn := func() bool { return true }
	prev := SetSampleForTest(trueFn)
	defer SetSampleForTest(prev)

	if got := SampleForTest(); reflect.ValueOf(got).Pointer() != reflect.ValueOf(trueFn).Pointer() {
		t.Error("SampleForTest must expose the installed sampler fn")
	}

	r, err := New(testRegistry(t), []RuleDef{
		{Name: "mined_active", Pattern: "/ping", Tool: "read_csv",
			ArgsTemplate: `{"path":"testdata/sample.csv"}`, Origin: "mined", State: RuleActive},
		{Name: "shadow_rule", Pattern: "/shadow", Tool: "read_csv",
			ArgsTemplate: `{"path":"testdata/sample.csv"}`, Origin: "mined", State: RuleShadow},
		{Name: "seed_rule", Pattern: "/seed", Tool: "read_csv",
			ArgsTemplate: `{"path":"testdata/sample.csv"}`, Origin: "seed", State: RuleActive},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Sampler true → the active mined rule answers AND requests the LLM
	// double-check (the 1% demotion guard).
	d := r.Handle(context.Background(), "/ping")
	if !d.Handled || d.RuleID != "mined_active" || d.Err != nil {
		t.Fatalf("sampled mined rule: %+v", d)
	}
	if !d.Evaluate || !d.AnswerFromRule {
		t.Errorf("sampling forced: Evaluate=%v AnswerFromRule=%v, want true/true", d.Evaluate, d.AnswerFromRule)
	}
	if d.Output == nil || d.Text() == "" {
		t.Errorf("sampled rule must still produce the answer: %+v", d)
	}

	// Sampler false → no evaluation: the promoted rule serves unchecked.
	SetSampleForTest(func() bool { return false })
	d = r.Handle(context.Background(), "/ping")
	if !d.Handled || d.RuleID != "mined_active" {
		t.Fatalf("unsampled mined rule: %+v", d)
	}
	if d.Evaluate || d.AnswerFromRule {
		t.Errorf("sampler false: Evaluate=%v AnswerFromRule=%v, want false/false", d.Evaluate, d.AnswerFromRule)
	}

	// Shadow rules always evaluate, and the LLM (not the rule) answers.
	d = r.Handle(context.Background(), "/shadow")
	if !d.Handled || d.RuleID != "shadow_rule" {
		t.Fatalf("shadow rule: %+v", d)
	}
	if !d.Evaluate || d.AnswerFromRule {
		t.Errorf("shadow rule: Evaluate=%v AnswerFromRule=%v, want true/false", d.Evaluate, d.AnswerFromRule)
	}

	// Seed-origin rules never sample, whatever the sampler says.
	SetSampleForTest(func() bool { return true })
	d = r.Handle(context.Background(), "/seed")
	if !d.Handled || d.RuleID != "seed_rule" {
		t.Fatalf("seed rule: %+v", d)
	}
	if d.Evaluate || d.AnswerFromRule {
		t.Errorf("seed rule must never sample: Evaluate=%v AnswerFromRule=%v", d.Evaluate, d.AnswerFromRule)
	}
}

func TestHandlePromptTooLong(t *testing.T) {
	r, err := New(testRegistry(t), Seeded())
	if err != nil {
		t.Fatal(err)
	}
	// Exact boundary, shaped like a routed command: /csv_head's \S+ capture
	// is padded so the prompt lands at exactly MaxPrompt bytes and still
	// matches (the cap is >, not >=). The padded path then fails inside the
	// tool — fine, the point is the rule was reached at all.
	const head = "/csv_head "
	atCap := head + strings.Repeat("a", MaxPrompt-len(head))
	if len(atCap) != MaxPrompt {
		t.Fatalf("at-cap prompt length = %d, want exactly %d", len(atCap), MaxPrompt)
	}
	if d := r.Handle(context.Background(), atCap); !d.Handled || d.RuleID != "csv_head" {
		t.Errorf("exactly MaxPrompt bytes must still route: %+v", d)
	}

	// One byte over the cap, same command shape: refused with an empty
	// Decision — oversized prompts are not command-shaped and skip to the
	// LLM path.
	overCap := head + strings.Repeat("a", MaxPrompt-len(head)+1)
	if len(overCap) != MaxPrompt+1 {
		t.Fatalf("over-cap prompt length = %d, want exactly %d", len(overCap), MaxPrompt+1)
	}
	d := r.Handle(context.Background(), overCap)
	if d.Handled || d.RuleID != "" || d.Evaluate || d.Err != nil {
		t.Errorf("over-cap prompt must return an empty Decision: %+v", d)
	}
	if d.Text() != "" {
		t.Errorf("empty decision text = %q, want empty", d.Text())
	}

	// Unmarshalable output falls back to fmt.Sprint instead of erroring.
	if got := (Decision{Output: make(chan int)}).Text(); got == "" {
		t.Error("Text fallback for unmarshalable output returned empty string")
	}
}
