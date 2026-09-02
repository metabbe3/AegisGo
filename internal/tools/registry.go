// Package tools provides AegisGo's tool set. Every tool implements the
// common Tool interface (tool.go): one typed handler, invoked either
// deterministically via Execute (router/CLI) or through the agent framework
// via FuncTool (LLM). File tools are sandboxed to a workspace root — paths
// are validated per call in pathutil.go, a security boundary.
package tools

import (
	"fmt"
)

// Options configures the built-in tool set.
type Options struct {
	// Workspace is the root file tools may read from (security boundary).
	Workspace string
	// SQL configures the sql_query tool (embedded SQLite by default).
	SQL SQLOptions
	// Download bounds and configures the download tool. Zero values take
	// safe defaults; negative values fail Builtin loudly.
	Download DownloadOptions
}

// Builtin returns the built-in tool set. Order is stable; tools are
// appended, never replaced, so provider-side tool lists stay diffable.
func Builtin(opts Options) ([]Tool, error) {
	readCSV, err := NewReadCSV(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("building read_csv: %w", err)
	}
	csvStats, err := NewCSVStats(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("building csv_stats: %w", err)
	}
	readDoc, err := NewReadDoc(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("building read_doc: %w", err)
	}
	sysCmd, err := NewSystemCommand()
	if err != nil {
		return nil, fmt.Errorf("building system_command: %w", err)
	}
	sqlQuery, err := NewSQLQuery(opts.Workspace, opts.SQL)
	if err != nil {
		return nil, fmt.Errorf("building sql_query: %w", err)
	}
	makeDir, err := NewMakeDir(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("building make_dir: %w", err)
	}
	listDir, err := NewListDir(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("building list_dir: %w", err)
	}
	// One job manager serves download + job_status so started jobs are
	// pollable through the same process.
	jobs := NewJobManager()
	download, err := NewDownload(opts.Workspace, jobs, opts.Download)
	if err != nil {
		return nil, fmt.Errorf("building download: %w", err)
	}
	jobStatus, err := NewJobStatus(jobs)
	if err != nil {
		return nil, fmt.Errorf("building job_status: %w", err)
	}
	return []Tool{readCSV, csvStats, readDoc, sysCmd, sqlQuery,
		makeDir, listDir, download, jobStatus}, nil
}
