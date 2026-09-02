package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolvePath resolves a tool-supplied path against the workspace root and
// guarantees the result stays inside it. This is a security boundary: model
// output (or prompt-injected model output) must never steer a tool at files
// outside the workspace — no absolute escapes, no ../ walks, no symlinks out.
func resolvePath(workspace, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("path is required")
	}
	// Reject home-dir and env expansion outright; tools get plain relative
	// (or in-workspace absolute) paths only.
	if strings.HasPrefix(name, "~") || strings.Contains(name, "$") {
		return "", fmt.Errorf("path %q must not use ~ or variable expansion", name)
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolving workspace: %w", err)
	}

	var candidate string
	if filepath.IsAbs(name) {
		candidate = filepath.Clean(name)
	} else {
		candidate = filepath.Join(root, name)
	}

	// Resolve symlinks before the containment check so a link pointing
	// outside the workspace is caught.
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path %q not found", name)
		}
		return "", fmt.Errorf("resolving path %q: %w", name, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", name)
	}
	return real, nil
}

// resolveNewPath validates a path whose final components may not exist yet
// (make_dir targets, download destinations) with the same containment
// guarantee as resolvePath. EvalSymlinks cannot see through a path that
// isn't there, so the deepest EXISTING ancestor is resolved instead and the
// non-existent suffix is rejoined onto it: suffix components cannot hide a
// symlink (they don't exist), and the ancestor is contained exactly the way
// resolvePath checks. The result is rebuilt from the resolved ancestor —
// never the raw joined path — so a symlinked workspace root (macOS
// /var → /private/var) still yields a real, contained path. The same TOCTOU
// window resolvePath already has applies: a component swapped in between
// validation and use is out of scope for a single-process agent.
func resolveNewPath(workspace, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(name, "~") || strings.Contains(name, "$") {
		return "", fmt.Errorf("path %q must not use ~ or variable expansion", name)
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolving workspace: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}

	var candidate string
	if filepath.IsAbs(name) {
		candidate = filepath.Clean(name)
	} else {
		candidate = filepath.Join(root, name)
	}

	// Walk up to the deepest existing ancestor, collecting the not-yet-real
	// suffix. ENOTDIR (traversing through a regular file) is not IsNotExist
	// and surfaces as a resolve error, mirroring resolvePath.
	ancestor := candidate
	var suffix []string
	for {
		_, statErr := os.Stat(ancestor)
		if statErr == nil {
			break
		}
		if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("resolving path %q: %w", name, statErr)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			// Reached the filesystem root without finding anything — the
			// containment check below can never pass for this path.
			return "", fmt.Errorf("path %q escapes the workspace", name)
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}

	realAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %w", name, err)
	}
	if realAncestor != realRoot && !strings.HasPrefix(realAncestor, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", name)
	}
	if len(suffix) == 0 {
		return realAncestor, nil
	}
	return filepath.Join(append([]string{realAncestor}, suffix...)...), nil
}
