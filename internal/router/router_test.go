package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
