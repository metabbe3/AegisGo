package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeDirCreates(t *testing.T) {
	dir := t.TempDir()
	tl, err := NewMakeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got DirMakeOutput
	callTool(t, "make_dir", tl, `{"path":"data/reports/2026"}`, &got)
	if got.Path != "data/reports/2026" || got.Existed {
		t.Errorf("output = %+v, want created fresh", got)
	}
	if st, err := os.Stat(filepath.Join(dir, "data", "reports", "2026")); err != nil || !st.IsDir() {
		t.Errorf("dir on disk: %v (%v)", st, err)
	}
}

func TestMakeDirIdempotent(t *testing.T) {
	dir := t.TempDir()
	tl, err := NewMakeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var first, second DirMakeOutput
	callTool(t, "make_dir", tl, `{"path":"once"}`, &first)
	callTool(t, "make_dir", tl, `{"path":"once"}`, &second)
	if first.Existed || !second.Existed {
		t.Errorf("first=%+v second=%+v, want existed=false then true", first, second)
	}
}

func TestMakeDirEscapesWorkspace(t *testing.T) {
	dir := t.TempDir()
	tl, err := NewMakeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../escape", "/tmp/aegis-escape"} {
		if _, err := tl.Execute(context.Background(), []byte(`{"path":`+quoteJSON(path)+`}`)); err == nil {
			t.Errorf("path %q accepted", path)
		}
	}
	// A symlink inside the workspace pointing out: mkdir through it must
	// fail, not create outside.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"path":"leak/new"}`)); err == nil {
		t.Error("mkdir through out-of-workspace symlink accepted")
	} else if _, statErr := os.Stat(filepath.Join(outside, "new")); statErr == nil {
		t.Error("escape created a directory outside the workspace")
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWSFile(t, dir, "a.txt", "hello")
	writeWSFile(t, dir, "b.md", "longer content")
	tl, err := NewListDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var got DirListOutput
	callTool(t, "list_dir", tl, `{"path":"."}`, &got)
	if got.Total != 3 || len(got.Entries) != 3 || got.Truncated {
		t.Fatalf("output = %+v, want all 3 entries", got)
	}
	byName := map[string]DirEntryInfo{}
	for _, e := range got.Entries {
		byName[e.Name] = e
	}
	if !byName["docs"].IsDir {
		t.Errorf("docs.IsDir = false, want true")
	}
	if byName["a.txt"].IsDir || byName["a.txt"].SizeBytes != 5 {
		t.Errorf("a.txt = %+v, want file of 5 bytes", byName["a.txt"])
	}
	if byName["b.md"].SizeBytes != 14 {
		t.Errorf("b.md size = %d, want 14", byName["b.md"].SizeBytes)
	}
}

func TestListDirBounds(t *testing.T) {
	dir := t.TempDir()
	// 600 entries: default caps at 100, explicit 2 caps at 2, an explicit
	// request above the hard cap clamps to 500.
	for i := 0; i < 600; i++ {
		writeWSFile(t, dir, fmt.Sprintf("f%03d", i), "x")
	}
	tl, err := NewListDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		args      string
		wantLen   int
		wantTrunc bool
	}{
		{`{"path":"."}`, 100, true},
		{`{"path":".","max_entries":2}`, 2, true},
		{`{"path":".","max_entries":1000}`, listDirHardCap, true},
		{`{"path":".","max_entries":6000}`, listDirHardCap, true},
	}
	for _, tc := range cases {
		var got DirListOutput
		callTool(t, "list_dir", tl, tc.args, &got)
		if len(got.Entries) != tc.wantLen || got.Total != 600 || got.Truncated != tc.wantTrunc {
			t.Errorf("%s: entries=%d total=%d truncated=%v, want %d/600/%v",
				tc.args, len(got.Entries), got.Total, got.Truncated, tc.wantLen, tc.wantTrunc)
		}
	}
}

func TestListDirErrors(t *testing.T) {
	dir := t.TempDir()
	writeWSFile(t, dir, "f.txt", "hi")
	tl, err := NewListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, wantErr string
	}{
		{"missing", "not found"},
		{"f.txt", "listing"}, // listing a file, not a directory
	}
	// "../../etc" does not exist from the temp workspace, so resolvePath
	// reports "not found" — still a rejection. The escape wording itself is
	// pinned in pathutil_test (symlink + traversal cases).
	if _, err := tl.Execute(context.Background(), []byte(`{"path":"../../etc"}`)); err == nil {
		t.Error("../../etc accepted, want rejection")
	}
	for _, tc := range cases {
		_, err := tl.Execute(context.Background(), []byte(`{"path":`+quoteJSON(tc.path)+`}`))
		if err == nil {
			t.Errorf("%s: accepted, want error", tc.path)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.path, err, tc.wantErr)
		}
	}
}

// TestDirToolDualEntryParity: same args through Execute and FuncTool().Call
// produce the same result (the §6 contract), for both dir tools.
func TestDirToolDualEntryParity(t *testing.T) {
	dir := t.TempDir()

	mkdir, err := NewMakeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	viaExec, err := mkdir.Execute(context.Background(), []byte(`{"path":"p/exec"}`))
	if err != nil {
		t.Fatal(err)
	}
	viaLLM, err := mkdir.FuncTool().Call(context.Background(), `{"path":"p/llm"}`)
	if err != nil {
		t.Fatal(err)
	}
	execOut, ok1 := viaExec.(DirMakeOutput)
	llmOut, ok2 := viaLLM.(DirMakeOutput)
	if !ok1 || !ok2 {
		t.Fatalf("types: %T vs %T", viaExec, viaLLM)
	}
	if execOut.Existed != llmOut.Existed || execOut.Path != "p/exec" || llmOut.Path != "p/llm" {
		t.Errorf("divergence: exec=%+v llm=%+v", execOut, llmOut)
	}

	ls, err := NewListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	viaExec, err = ls.Execute(context.Background(), []byte(`{"path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	viaLLM, err = ls.FuncTool().Call(context.Background(), `{"path":"."}`)
	if err != nil {
		t.Fatal(err)
	}
	lsExec, ok1 := viaExec.(DirListOutput)
	lsLLM, ok2 := viaLLM.(DirListOutput)
	if !ok1 || !ok2 {
		t.Fatalf("types: %T vs %T", viaExec, viaLLM)
	}
	if lsExec.Total != lsLLM.Total || len(lsExec.Entries) != len(lsLLM.Entries) {
		t.Errorf("divergence: exec=%+v llm=%+v", lsExec, lsLLM)
	}
}
