package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDoc(t *testing.T) {
	tl, err := NewReadDoc(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name() != "read_doc" {
		t.Errorf("name = %q", tl.Name())
	}

	var got DocReadOutput
	callTool(t, "read_doc", tl, `{"path":"testdata/sample.csv"}`, &got)
	if got.Size == 0 || got.Truncated {
		t.Errorf("size=%d truncated=%v", got.Size, got.Truncated)
	}
	if len(got.Content) == 0 || !strings.Contains(got.Content, "order_id") {
		t.Error("content missing CSV header")
	}
}

func TestReadDocTruncates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	tl, err := NewReadDoc(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got DocReadOutput
	callTool(t, "read_doc", tl, `{"path":"big.txt","max_bytes":10}`, &got)
	if !got.Truncated || len(got.Content) != 10 {
		t.Errorf("truncated=%v len=%d", got.Truncated, len(got.Content))
	}

	// offset paging reads past the truncation point
	callTool(t, "read_doc", tl, `{"path":"big.txt","max_bytes":10,"offset":90}`, &got)
	if got.Truncated || len(got.Content) != 10 || got.Offset != 90 {
		t.Errorf("offset read: truncated=%v len=%d offset=%d", got.Truncated, len(got.Content), got.Offset)
	}
}

func TestReadDocRejectsBinaryAndEscapes(t *testing.T) {
	tl, err := NewReadDoc(workspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"path":"testdata/sample.pdf"}`)); err == nil {
		t.Error("expected extension rejection")
	}
	if _, err := tl.Execute(context.Background(), []byte(`{"path":"/etc/passwd"}`)); err == nil {
		t.Error("expected workspace-escape rejection")
	}
}

func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	if p, err := resolvePath(dir, "sub/f.txt"); err != nil || filepath.Base(p) != "f.txt" {
		t.Errorf("relative: p=%v err=%v", p, err)
	}
	if p, err := resolvePath(dir, filepath.Join(dir, "sub", "f.txt")); err != nil || filepath.Base(p) != "f.txt" {
		t.Errorf("absolute inside: p=%v err=%v", p, err)
	}
	if _, err := resolvePath(dir, "../outside.txt"); err == nil {
		t.Error("parent escape not rejected")
	}
	if _, err := resolvePath(dir, "~/x"); err == nil {
		t.Error("home path not rejected")
	}
	if _, err := resolvePath(dir, "missing.txt"); err == nil {
		t.Error("missing file not rejected")
	}
}
