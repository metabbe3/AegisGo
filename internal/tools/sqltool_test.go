package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newSQLTool(t *testing.T, mode string) Tool {
	t.Helper()
	dir := t.TempDir()
	// ro mode refuses to create a missing file (correct behavior); seed it.
	path := filepath.Join(dir, "tool.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tl, err := NewSQLQuery(workspace(t), SQLOptions{Path: path, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestSQLPolicy(t *testing.T) {
	ro := newSQLTool(t, "ro")
	ctx := context.Background()

	// Multi-statement rejected.
	if _, err := ro.Execute(ctx, []byte(`{"query":"SELECT 1; DROP TABLE x"}`)); err == nil {
		t.Error("multi-statement accepted")
	}
	// String literal with no placeholders rejected (injection shape).
	if _, err := ro.Execute(ctx, []byte(`{"query":"SELECT * FROM t WHERE a = 'x'"}`)); err == nil {
		t.Error("inline literal accepted")
	}
	// Placeholder/arg count mismatch rejected.
	if _, err := ro.Execute(ctx, []byte(`{"query":"SELECT ?","args":[]}`)); err == nil {
		t.Error("placeholder mismatch accepted")
	}
	// Writes rejected in ro mode by policy…
	if _, err := ro.Execute(ctx, []byte(`{"query":"INSERT INTO t VALUES (?)"}`)); err == nil {
		t.Error("write accepted in ro mode")
	}
	// …and a valid read works.
	var out SQLOutput
	callTool(t, "sql_query", ro, `{"query":"SELECT 1 + 1 AS n"}`, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] == nil {
		t.Errorf("select 1+1 = %+v", out)
	}
}

func TestSQLReadOnlyEngine(t *testing.T) {
	// rw tool writes a table; ro tool (same file) must not be able to write
	// even though the policy layer alone might miss something.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shared.db")

	rw, err := NewSQLQuery(workspace(t), SQLOptions{Path: dbPath, Mode: "rw"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rw.Execute(ctx, []byte(`{"query":"CREATE TABLE demo (k TEXT)"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rw.Execute(ctx, []byte(`{"query":"INSERT INTO demo VALUES (?)","args":["a"]}`)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ro, err := NewSQLQuery(workspace(t), SQLOptions{Path: dbPath, Mode: "ro"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Execute(ctx, []byte(`{"query":"CREATE TABLE evil (k TEXT)"}`)); err == nil {
		t.Error("ro engine accepted CREATE")
	}
	var out SQLOutput
	callTool(t, "sql_query", ro, `{"query":"SELECT COUNT(*) AS n FROM demo"}`, &out)
	if got := out.Rows[0][0]; got == nil {
		t.Error("count read failed")
	}
}

func TestSQLAttachCSV(t *testing.T) {
	tl := newSQLTool(t, "ro")
	var out SQLOutput
	callTool(t, "sql_query", tl, `{
		"query": "SELECT c06_shipped, COUNT(*) AS orders, SUM(CAST(c04_qty AS INTEGER)) AS units FROM orders WHERE c06_shipped = ? GROUP BY c06_shipped",
		"args": ["true"],
		"attach_csvs": [{"name": "orders", "path": "testdata/sample.csv"}]
	}`, &out)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %+v", out.Rows)
	}
	// 6 shipped orders in testdata/sample.csv; sum(qty) for shipped=true.
	if out.Rows[0][1] == nil || out.Rows[0][2] == nil {
		t.Fatalf("aggregates nil: %+v", out.Rows)
	}

	// Reserved names and bad identifiers rejected.
	if _, err := tl.Execute(context.Background(), []byte(`{
		"query": "SELECT 1",
		"attach_csvs": [{"name": "order", "path": "testdata/sample.csv"}]
	}`)); err == nil {
		t.Error("reserved table name accepted")
	}
}

func TestSQLRowCap(t *testing.T) {
	tl := newSQLTool(t, "ro")
	var out SQLOutput
	// generate >200 rows via recursive CTE
	callTool(t, "sql_query", tl,
		`{"query":"WITH RECURSIVE cnt(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM cnt WHERE x<1000) SELECT x FROM cnt"}`,
		&out)
	if len(out.Rows) != sqlMaxRows || !out.Truncated {
		t.Errorf("rows=%d truncated=%v", len(out.Rows), out.Truncated)
	}
}
