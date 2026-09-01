package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// docMaxBytes is the default (and hard) cap on read_doc output. Keeping tool
// results bounded is the cheapest defense against blowing the model context
// on a huge file.
const (
	docDefaultBytes = 32 * 1024
	docHardCap      = 256 * 1024
)

// docExtensions is the allowlist of text-oriented files read_doc will open.
var docExtensions = map[string]bool{
	".txt": true, ".md": true, ".json": true, ".log": true,
	".yaml": true, ".yml": true, ".xml": true, ".html": true, ".csv": true,
}

// DocReadInput is the schema for the read_doc tool.
type DocReadInput struct {
	// Path of the document, relative to the workspace root.
	Path string `json:"path"`
	// MaxBytes caps the returned content (default 32768, hard cap 262144).
	MaxBytes *int `json:"max_bytes,omitempty"`
	// Offset skips the first N bytes of the file (for paging big files).
	Offset *int `json:"offset,omitempty"`
}

// DocReadOutput is the result of read_doc.
type DocReadOutput struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Offset    int    `json:"offset"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

// NewReadDoc builds the read_doc tool bound to a workspace root.
func NewReadDoc(workspace string) (tool.FuncTool, error) {
	return functool.New(functool.Config{
		Name:        "read_doc",
		Description: "Read a text document from the workspace (txt, md, json, log, yaml, xml, html, csv). Returns up to max_bytes of content; use offset to page through larger files.",
	}, func(ctx context.Context, in DocReadInput) (DocReadOutput, error) {
		return readDoc(workspace, in)
	})
}

func readDoc(workspace string, in DocReadInput) (DocReadOutput, error) {
	path, err := resolvePath(workspace, in.Path)
	if err != nil {
		return DocReadOutput{}, err
	}
	if !docExtensions[strings.ToLower(filepath.Ext(path))] {
		return DocReadOutput{}, fmt.Errorf("read_doc does not support %q files (supported: txt, md, json, log, yaml, yml, xml, html, csv)", filepath.Ext(path))
	}

	limit := docDefaultBytes
	if in.MaxBytes != nil && *in.MaxBytes > 0 {
		limit = min(*in.MaxBytes, docHardCap)
	}
	offset := 0
	if in.Offset != nil && *in.Offset > 0 {
		offset = *in.Offset
	}

	f, err := os.Open(path) //nolint:gosec // path already validated against workspace
	if err != nil {
		return DocReadOutput{}, fmt.Errorf("opening %q: %w", in.Path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return DocReadOutput{}, fmt.Errorf("stat %q: %w", in.Path, err)
	}
	if st.IsDir() {
		return DocReadOutput{}, fmt.Errorf("%q is a directory", in.Path)
	}
	if offset > 0 {
		if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
			return DocReadOutput{}, fmt.Errorf("seek %q: %w", in.Path, err)
		}
	}

	buf := make([]byte, limit+1) // +1 so we can detect truncation
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return DocReadOutput{}, fmt.Errorf("reading %q: %w", in.Path, err)
	}
	truncated := n > limit
	if truncated {
		n = limit
	}

	slog.Debug("read_doc", "path", in.Path, "bytes", n, "truncated", truncated)
	return DocReadOutput{
		Path:      in.Path,
		Size:      st.Size(),
		Offset:    offset,
		Truncated: truncated,
		Content:   string(buf[:n]),
	}, nil
}

// openCSV opens a CSV file that resolvePath has already validated.
func openCSV(path string) (*os.File, error) {
	if strings.ToLower(filepath.Ext(path)) != ".csv" {
		return nil, fmt.Errorf("%q is not a CSV file", filepath.Base(path))
	}
	f, err := os.Open(path) //nolint:gosec // path already validated against workspace
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	return f, nil
}
