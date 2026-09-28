package tools

import (
	"context"
	"fmt"
	"strings"

	"aegisgo/internal/loganalysis"
)

// AnalyzeLogToolInput is the schema for analyze_log. Field comments are
// the LLM-facing description (functool derives them) — keep them honest.
type AnalyzeLogToolInput struct {
	// Path of the log file to analyze, relative to the workspace root.
	Path string `json:"path"`
	// Kind of analysis: "perf" (API latency/5xx), "exceptions" (error
	// templates), "access" (IPs/routes/status), "behaviour" (actions,
	// peak hours, bursts). Default "perf".
	Kind string `json:"kind,omitempty"`
	// Lines window from the end of the file. Default 500, max 20000.
	MaxLines int `json:"max_lines,omitempty"`
}

// AnalyzeLogToolOutput is a flat summary (LLM-friendly, no nested maps).
type AnalyzeLogToolOutput struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	LinesRead int    `json:"lines_read"`
	Summary   string `json:"summary"`
}

// analyzeLogRuns maps the public kinds to their computation.
func analyzeLogRuns(kind string, path string, n int) (lines int, summary string, err error) {
	switch kind {
	case "perf":
		r, err := loganalysis.AnalyzeAPIPerf(path, n)
		if err != nil {
			return 0, "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d requests, p50 %.0fms, p95 %.0fms, slow(>1s) %d, 5xx %d (%.1f%%)",
			r.Requests, r.P50, r.P95, r.Slow, r.Errors, r.ErrPct)
		for i, kv := range r.Slowest {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "; slowest %s", kv.Key)
		}
		return r.LinesRead, b.String(), nil
	case "exceptions":
		r, err := loganalysis.AnalyzeExceptions(path, n)
		if err != nil {
			return 0, "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d/%d error-ish lines", r.ErrorLines, r.LinesRead)
		for i, kv := range r.Top {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "; %dx %s", kv.Count, clampString(kv.Key, 80))
		}
		return r.LinesRead, b.String(), nil
	case "access":
		r, err := loganalysis.AnalyzeAccess(path, n)
		if err != nil {
			return 0, "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d hits", r.Hits)
		for i, kv := range r.TopIPs {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "; top ip %s x%d", kv.Key, kv.Count)
		}
		for i, kv := range r.TopPaths {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "; route %s x%d", kv.Key, kv.Count)
		}
		return r.LinesRead, b.String(), nil
	case "behaviour":
		r, err := loganalysis.AnalyzeBehaviour(path, n)
		if err != nil {
			return 0, "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d events, %d novel shapes", r.Actions, r.NewShapes)
		for i, kv := range r.TopCmds {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "; %s x%d", kv.Key, kv.Count)
		}
		return r.LinesRead, b.String(), nil
	}
	return 0, "", fmt.Errorf("unknown kind %q (want perf|exceptions|access|behaviour)", kind)
}

func clampString(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// NewAnalyzeLog builds the dual-entry log analysis tool. The workspace
// containment is the SAME security boundary as read_doc: resolvePath
// rejects escapes and symlinks pointing outside (Hard Rule 2).
func NewAnalyzeLog(workspace string) (Tool, error) {
	return New(Config{
		Name:        "analyze_log",
		Description: "Analyze a log file deterministically (no LLM): kind=perf (API latency p50/p95, slow endpoints, 5xx %), exceptions (error templates), access (top IPs/routes/status), behaviour (actions, peak hours, bursts).",
	}, func(ctx context.Context, in AnalyzeLogToolInput) (AnalyzeLogToolOutput, error) {
		out := AnalyzeLogToolOutput{}
		if in.Path == "" {
			return out, fmt.Errorf("path is required")
		}
		kind := in.Kind
		if kind == "" {
			kind = "perf"
		}
		n := optPos(&in.MaxLines, 500)
		if n > 20000 {
			n = 20000
		}
		// Security boundary: same resolvePath as read_csv/read_doc.
		real, err := resolvePath(workspace, in.Path)
		if err != nil {
			return out, err
		}
		lines, summary, err := analyzeLogRuns(kind, real, n)
		if err != nil {
			return out, err
		}
		return AnalyzeLogToolOutput{Kind: kind, Path: in.Path, LinesRead: lines, Summary: summary}, nil
	})
}
