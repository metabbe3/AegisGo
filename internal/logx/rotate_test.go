package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotatingWriterRotatesAtCap: writes past MaxBytes produce a rotated
// file and a fresh active file; the active file only holds post-rotation
// content.
func TestRotatingWriterRotatesAtCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aegis.log")
	w := &RotatingWriter{Path: path, MaxBytes: 64, KeepFiles: 2}

	if _, err := w.Write([]byte(strings.Repeat("a", 50))); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := w.Write([]byte(strings.Repeat("b", 50))); err != nil { // crosses 64
		t.Fatalf("write 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rotated, _ := filepath.Glob(path + ".*")
	if len(rotated) != 1 {
		t.Fatalf("rotated files = %d, want 1 (%v)", len(rotated), rotated)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if string(active) != strings.Repeat("b", 50) {
		t.Fatalf("active content = %q, want fresh post-rotation write", active)
	}
	r0, _ := os.ReadFile(rotated[0])
	if string(r0) != strings.Repeat("a", 50) {
		t.Fatalf("rotated content = %q", r0)
	}
}

// TestRotatingWriterPrunesToKeep: retention caps rotated files at KeepFiles.
func TestRotatingWriterPrunesToKeep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aegis.log")
	w := &RotatingWriter{Path: path, MaxBytes: 10, KeepFiles: 2}
	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 11))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	w.Close()

	rotated, _ := filepath.Glob(path + ".*")
	if len(rotated) != 2 {
		t.Fatalf("rotated files = %d, want 2 (retention)", len(rotated))
	}
}

// TestRotatingWriterZeroValueNeverWrittenNoDisk: a writer that is never
// written to never creates the file (lazy open contract).
func TestRotatingWriterZeroValueNeverWrittenNoDisk(t *testing.T) {
	dir := t.TempDir()
	w := &RotatingWriter{Path: filepath.Join(dir, "never.log")}
	w.Close() // close on never-opened writer must be a no-op, not a panic
	if _, err := os.Stat(filepath.Join(dir, "never.log")); !os.IsNotExist(err) {
		t.Fatalf("file should not exist: %v", err)
	}
}
