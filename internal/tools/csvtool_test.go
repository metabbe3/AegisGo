package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// callTool invokes a FuncTool with a JSON argument body, decoding the result
// into out.
func callTool(t *testing.T, name string, f any, args string, out any) {
	t.Helper()
	type funcTool interface {
		Call(ctx context.Context, args string) (any, error)
	}
	ft, ok := f.(funcTool)
	if !ok {
		t.Fatalf("%s: not a FuncTool: %T", name, f)
	}
	res, err := ft.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("%s: Call: %v", name, err)
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
	_, err = tl.Call(context.Background(), `{"path":"../../etc/passwd"}`)
	if err == nil {
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
	set, err := Builtin(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read_csv", "csv_stats", "read_doc"}
	if len(set) != len(want) {
		t.Fatalf("got %d tools, want %d", len(set), len(want))
	}
	for i, name := range want {
		if set[i].Name() != name {
			t.Errorf("tool[%d] = %q, want %q", i, set[i].Name(), name)
		}
	}
}
