// Package tools provides AegisGo's built-in file tools. Every tool is a
// stateless functool.FuncTool bound to a workspace root; paths are validated
// per call (see pathutil.go) so the model can never read outside it.
package tools

import (
	"fmt"

	"github.com/microsoft/agent-framework-go/tool"
)

// Builtin returns the built-in tool set for a workspace root. Order is
// stable; tools are appended, never replaced, so provider-side tool lists
// stay diffable.
func Builtin(workspace string) ([]tool.Tool, error) {
	readCSV, err := NewReadCSV(workspace)
	if err != nil {
		return nil, fmt.Errorf("building read_csv: %w", err)
	}
	csvStats, err := NewCSVStats(workspace)
	if err != nil {
		return nil, fmt.Errorf("building csv_stats: %w", err)
	}
	readDoc, err := NewReadDoc(workspace)
	if err != nil {
		return nil, fmt.Errorf("building read_doc: %w", err)
	}
	return []tool.Tool{readCSV, csvStats, readDoc}, nil
}
