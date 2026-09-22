package tools

import (
	"context"
	"fmt"
	"os"
)

// listDirDefault and listDirHardCap bound list_dir output (Hard Rule 10: a
// huge directory must not blow the model context or RAM).
const (
	listDirDefault = 100
	listDirHardCap = 500
)

// DirMakeInput is the schema for the make_dir tool.
type DirMakeInput struct {
	// Path of the directory to create, relative to the workspace root.
	// Missing parents are created as needed; the path can never escape the
	// workspace.
	Path string `json:"path"`
}

// DirMakeOutput is the result of make_dir.
type DirMakeOutput struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
}

// DirListInput is the schema for the list_dir tool.
type DirListInput struct {
	// Path of the directory to list, relative to the workspace root.
	Path string `json:"path"`
	// MaxEntries caps how many entries are returned (default 100, max 500).
	MaxEntries *int `json:"max_entries,omitempty"`
}

// DirListOutput is the result of list_dir.
type DirListOutput struct {
	Path      string         `json:"path"`
	Entries   []DirEntryInfo `json:"entries"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

// DirEntryInfo describes one directory entry.
type DirEntryInfo struct {
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	SizeBytes int64  `json:"size_bytes"`
}

// NewMakeDir builds the make_dir tool bound to a workspace root.
func NewMakeDir(workspace string) (Tool, error) {
	return New(Config{
		Name:        "make_dir",
		Description: "Create a directory inside the workspace, creating missing parents along the way. Returns whether the directory already existed.",
	}, func(ctx context.Context, in DirMakeInput) (DirMakeOutput, error) {
		path, err := resolveNewPath(workspace, in.Path)
		if err != nil {
			return DirMakeOutput{}, err
		}
		existed := true
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			existed = false
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return DirMakeOutput{}, fmt.Errorf("creating %q: %w", in.Path, err)
		}
		return DirMakeOutput{Path: in.Path, Existed: existed}, nil
	})
}

// NewListDir builds the list_dir tool bound to a workspace root.
func NewListDir(workspace string) (Tool, error) {
	return New(Config{
		Name:        "list_dir",
		Description: "List a workspace directory: entry names, whether each is a directory, and file sizes. Use it to discover files before read_csv or read_doc.",
	}, func(ctx context.Context, in DirListInput) (DirListOutput, error) {
		limit := min(optInt(in.MaxEntries, listDirDefault), listDirHardCap)
		return listDir(workspace, in.Path, limit)
	})
}

func listDir(workspace, name string, limit int) (DirListOutput, error) {
	path, err := resolvePath(workspace, name)
	if err != nil {
		return DirListOutput{}, err
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return DirListOutput{}, fmt.Errorf("listing %q: %w", name, err)
	}
	// os.ReadDir sorts by name, so output is deterministic. Entries beyond
	// the limit are counted in Total, not stored.
	out := DirListOutput{Path: name, Total: len(ents), Truncated: len(ents) > limit,
		Entries: make([]DirEntryInfo, 0, min(len(ents), limit))}
	for _, e := range ents[:min(len(ents), limit)] {
		info := DirEntryInfo{Name: e.Name(), IsDir: e.IsDir()}
		if fi, infoErr := e.Info(); infoErr == nil {
			info.SizeBytes = fi.Size()
		}
		out.Entries = append(out.Entries, info)
	}
	return out, nil
}
