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

// TestResolveNewPath pins resolveNewPath's containment guarantees for paths
// that may not exist yet (make_dir targets, download destinations). Same
// security bar as resolvePath: every escape rejected loudly.
func TestResolveNewPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An in-workspace symlink to a directory OUTSIDE the workspace: the
	// classic mkdir escape (create inside the link).
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}

	t.Run("nested new path rejoins existing ancestor", func(t *testing.T) {
		p, err := resolveNewPath(dir, filepath.Join("sub", "a", "b", "c"))
		if err != nil {
			t.Fatalf("resolveNewPath: %v", err)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(real, "sub", "a", "b", "c"); p != want {
			t.Errorf("resolved = %q, want %q", p, want)
		}
	})
	t.Run("absolute in-workspace path", func(t *testing.T) {
		p, err := resolveNewPath(dir, filepath.Join(dir, "sub", "new"))
		if err != nil {
			t.Fatalf("resolveNewPath: %v", err)
		}
		if filepath.Base(p) != "new" || filepath.Base(filepath.Dir(p)) != "sub" {
			t.Errorf("resolved = %q", p)
		}
	})
	t.Run("workspace root itself is a valid target", func(t *testing.T) {
		p, err := resolveNewPath(dir, ".")
		if err != nil {
			t.Fatalf("resolveNewPath(dir, .): %v", err)
		}
		real, _ := filepath.EvalSymlinks(dir)
		if p != real {
			t.Errorf("dot = %q, want %q", p, real)
		}
	})
	t.Run("dotdot escape rejected", func(t *testing.T) {
		if _, err := resolveNewPath(dir, filepath.Join("..", "escape")); err == nil {
			t.Error("../escape accepted")
		}
		if _, err := resolveNewPath(dir, "/etc/newdir"); err == nil {
			t.Error("absolute outside path accepted")
		}
	})
	t.Run("symlinked ancestor out rejected", func(t *testing.T) {
		_, err := resolveNewPath(dir, filepath.Join("leak", "newdir"))
		if err == nil {
			t.Fatal("mkdir through an out-of-workspace symlink accepted")
		}
		if !strings.Contains(err.Error(), "escapes the workspace") {
			t.Errorf("error = %q, want escape wording", err)
		}
	})
	t.Run("symlinked workspace root stays real", func(t *testing.T) {
		// t.TempDir() on macOS lives under /var → /private/var, so dir may
		// itself contain a symlink. A second explicit symlink pins the
		// rebuild-from-resolved-ancestor behavior: the returned path must
		// be under the REAL workspace, not the symlinked spelling.
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "wslink")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		p, err := resolveNewPath(link, "brand/new/dir")
		if err != nil {
			t.Fatalf("resolveNewPath through symlinked root: %v", err)
		}
		if want := filepath.Join(real, "brand", "new", "dir"); p != want {
			t.Errorf("resolved = %q, want %q (rebuilt from real ancestor)", p, want)
		}
	})
	t.Run("traversal through a file errors", func(t *testing.T) {
		_, err := resolveNewPath(dir, filepath.Join("f.txt", "child"))
		if err == nil {
			t.Fatal("file-as-directory component accepted")
		}
		if !strings.Contains(err.Error(), "resolving path") {
			t.Errorf("error = %q, want resolving-path wording", err)
		}
	})
	t.Run("blank and expansion rejected", func(t *testing.T) {
		for _, bad := range []string{"", "  ", "~x", "$HOME/x"} {
			if _, err := resolveNewPath(dir, bad); err == nil {
				t.Errorf("path %q accepted", bad)
			}
		}
	})
	t.Run("missing workspace root surfaces", func(t *testing.T) {
		_, err := resolveNewPath(filepath.Join(dir, "gone"), "x")
		if err == nil {
			t.Fatal("missing workspace root accepted")
		}
		if !strings.Contains(err.Error(), "resolving workspace root") {
			t.Errorf("error = %q, want workspace-root wording", err)
		}
	})
}
