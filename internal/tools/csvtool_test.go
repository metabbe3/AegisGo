package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// callTool invokes a Tool via the deterministic Execute path, decoding the
// result into out.
func callTool(t *testing.T, name string, f Tool, args string, out any) {
	t.Helper()
	res, err := f.Execute(context.Background(), []byte(args))
	if err != nil {
		t.Fatalf("%s: Execute: %v", name, err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: unmarshal into %T: %v (raw: %s)", name, out, err, raw)
	}
}

func workspace(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // repo root, contains testdata/
}

func TestReadCSV(t *testing.T) {
	tl, err := NewReadCSV(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name() != "read_csv" {
		t.Errorf("name = %q", tl.Name())
	}

	var got CSVReadOutput
	callTool(t, "read_csv", tl, `{"path":"testdata/sample.csv"}`, &got)
	if len(got.Columns) != 7 {
		t.Errorf("columns = %v", got.Columns)
	}
	if got.TotalRows != 10 {
		t.Errorf("total rows = %d, want 10", got.TotalRows)
	}
	if len(got.Rows) != 10 { // under the 20-row preview default
		t.Errorf("preview rows = %d, want 10", len(got.Rows))
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
	if got.Rows[0][0] != "1001" || got.Rows[0][1] != "Acme Corp" {
		t.Errorf("first row = %v", got.Rows[0])
	}
}

func TestReadCSVMaxRows(t *testing.T) {
	tl, err := NewReadCSV(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	var got CSVReadOutput
	callTool(t, "read_csv", tl, `{"path":"testdata/sample.csv","max_rows":3}`, &got)
	if len(got.Rows) != 3 || got.TotalRows != 10 || !got.Truncated {
		t.Errorf("rows=%d total=%d truncated=%v", len(got.Rows), got.TotalRows, got.Truncated)
	}
}

func TestReadCSVEscapesWorkspace(t *testing.T) {
	tl, err := NewReadCSV(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"path":"../../etc/passwd"}`)); err == nil {
		t.Fatal("expected escape error, got nil")
	}
}

func TestCSVStats(t *testing.T) {
	tl, err := NewCSVStats(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	var got CSVStatsOutput
	callTool(t, "csv_stats", tl, `{"path":"testdata/sample.csv"}`, &got)
	if got.TotalRows != 10 || len(got.Columns) != 7 {
		t.Fatalf("rows=%d cols=%d", got.TotalRows, len(got.Columns))
	}
	byName := map[string]CSVColumn{}
	for _, c := range got.Columns {
		byName[c.Name] = c
	}
	if byName["qty"].Kind != "number" {
		t.Errorf("qty kind = %q, want number", byName["qty"].Kind)
	}
	if byName["customer"].Kind != "text" {
		t.Errorf("customer kind = %q, want text", byName["customer"].Kind)
	}
	if byName["ordered_at"].Kind != "date" {
		t.Errorf("ordered_at kind = %q, want date", byName["ordered_at"].Kind)
	}
}

func TestBuiltinRegistry(t *testing.T) {
	set, err := Builtin(Options{Workspace: workspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read_csv", "csv_stats", "read_doc", "system_command", "sql_query"}
	if len(set) != len(want) {
		t.Fatalf("got %d tools, want %d", len(set), len(want))
	}
	for i, name := range want {
		if set[i].Name() != name {
			t.Errorf("tool[%d] = %q, want %q", i, set[i].Name(), name)
		}
	}

	reg, err := NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("sql_query"); !ok {
		t.Error("sql_query not indexed")
	}
	if len(reg.FuncTools()) != len(want) {
		t.Errorf("FuncTools = %d, want %d", len(reg.FuncTools()), len(want))
	}
	if _, err := NewRegistry(append(set, set[0])...); err == nil {
		t.Error("duplicate tool names should be rejected")
	}
}

// TestDualEntryParity checks the §6 contract: the deterministic Execute
// path and the LLM FuncTool path run the same handler with the same result.
func TestDualEntryParity(t *testing.T) {
	tl, err := NewReadCSV(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	args := `{"path":"testdata/sample.csv","max_rows":3}`

	viaExec, err := tl.Execute(context.Background(), []byte(args))
	if err != nil {
		t.Fatal(err)
	}
	viaLLM, err := tl.FuncTool().Call(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	execOut, ok1 := viaExec.(CSVReadOutput)
	llmOut, ok2 := viaLLM.(CSVReadOutput)
	if !ok1 || !ok2 {
		t.Fatalf("types: %T vs %T", viaExec, viaLLM)
	}
	if execOut.TotalRows != llmOut.TotalRows || len(execOut.Rows) != len(llmOut.Rows) ||
		execOut.Columns[0] != llmOut.Columns[0] {
		t.Errorf("divergence: exec=%+v llm=%+v", execOut, llmOut)
	}
}
