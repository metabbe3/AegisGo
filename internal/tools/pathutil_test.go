package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin resolvePath's containment guarantees. resolvePath is a
// security boundary (CLAUDE.md rule 2): every escape must be rejected here,
// loudly, so a regression fails a named case rather than a live probe.
func TestResolvePathEdgeCases(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the workspace pointing outside it — the classic
	// model-steered escape resolvePath exists to stop.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}

	t.Run("empty path rejected", func(t *testing.T) {
		for _, empty := range []string{"", "   "} {
			if _, err := resolvePath(dir, empty); err == nil {
				t.Errorf("path %q accepted", empty)
			}
		}
	})
	t.Run("dot returns workspace root", func(t *testing.T) {
		p, err := resolvePath(dir, ".")
		if err != nil {
			t.Fatalf("resolvePath(dir, .): %v", err)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if p != real {
			t.Errorf("dot = %q, want workspace root %q", p, real)
		}
	})
	t.Run("variable expansion rejected", func(t *testing.T) {
		if _, err := resolvePath(dir, "$HOME/secret"); err == nil {
			t.Error("$-expanded path accepted")
		}
	})
	t.Run("subdirectory traversal stays inside", func(t *testing.T) {
		p, err := resolvePath(dir, filepath.Join("sub", "..", "sub", "f.txt"))
		if err != nil {
			t.Fatalf("traversal inside workspace: %v", err)
		}
		if filepath.Base(p) != "f.txt" {
			t.Errorf("resolved = %q, want f.txt", p)
		}
	})
	t.Run("symlink escape rejected", func(t *testing.T) {
		_, err := resolvePath(dir, "leak.txt")
		if err == nil {
			t.Fatal("symlink out of the workspace accepted")
		}
		if !strings.Contains(err.Error(), "escapes the workspace") {
			t.Errorf("error = %q, want escape wording", err)
		}
	})
}

func TestResolvePathNotADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A path that traverses THROUGH a regular file fails EvalSymlinks with
	// ENOTDIR, which is not os.IsNotExist — assert it reports a resolve
	// error rather than a misleading "not found".
	_, err := resolvePath(dir, filepath.Join("f.txt", "child"))
	if err == nil {
		t.Fatal("file-as-directory component accepted")
	}
	if !strings.Contains(err.Error(), "resolving path") {
		t.Errorf("error = %q, want resolving-path wording", err)
	}
}

func TestResolvePathMissingWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Candidate resolves fine, but the workspace root itself does not
	// exist: the root failure must surface, not a confusing path error.
	_, err := resolvePath(filepath.Join(dir, "gone"), filepath.Join(dir, "f.txt"))
	if err == nil {
		t.Fatal("missing workspace root accepted")
	}
	if !strings.Contains(err.Error(), "resolving workspace root") {
		t.Errorf("error = %q, want workspace-root wording", err)
	}
}
