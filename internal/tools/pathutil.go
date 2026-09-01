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
