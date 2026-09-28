// Package app — loganalysisgate.go: Telegram surface for the five
// deterministic log-analysis commands (owner 28 Sep):
//
//	/api_perf  <path> [-n 500]  — latency p50/p95, slow endpoints, 5xx %
//	/exceptions <path> [-n 500]  — error/panic templates grouped
//	/access    <path> [-n 500]   — IPs, routes, status codes, agents
//	/audit     [-n 200]          — the agent's OWN audit trail rollup
//	/behaviour <path> [-n 1000]  — repeated actions, hourly shape, bursts
//
// All L1-instant: pure computation, zero LLM. /audit reads the store,
// the rest read arbitrary files via the same path rules as /analyze.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"aegisgo/internal/loganalysis"
)

// LogAnalysisGate wires the commands to the store (for /audit) and the
// analyze_log tool (for every path-taking command — containment lives
// inside the tool, so the Telegram path and the router path share it).
type LogAnalysisGate struct {
	// QueryAuditRows returns the last N audit rows as maps.
	QueryAuditRows func(ctx context.Context, n int) ([]map[string]any, error)
	// ExecTool runs a registry tool by name (analyze_log); nil makes
	// path commands fall back to direct-file analysis (tests).
	ExecTool func(ctx context.Context, name string, args map[string]any) (any, error)
}

// analyzeViaTool runs analyze_log and renders its summary; false when
// the tool path is unavailable.
func (g *LogAnalysisGate) analyzeViaTool(ctx context.Context, kind, path string, n int) (string, bool) {
	if g.ExecTool == nil {
		return "", false
	}
	out, err := g.ExecTool(ctx, "analyze_log", map[string]any{
		"path": path, "kind": kind, "max_lines": n,
	})
	if err != nil {
		return "⚠️ " + err.Error(), true
	}
	// tool returns its flat output struct; render generically
	return fmt.Sprintf("📊 `%s` (%s)\n%s", path, kind, summarizeToolOutput(out)), true
}

// summarizeToolOutput renders the tool's flat output struct.
func summarizeToolOutput(out any) string {
	switch o := out.(type) {
	case map[string]any:
		if s, ok := o["summary"].(string); ok {
			return s
		}
	}
	// struct form (tools.AnalyzeLogToolOutput) — reflect-free render:
	b, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf("%v", out)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if s, ok := m["summary"].(string); ok {
			return s
		}
	}
	return string(b)
}

// pathAndN parses "<path> [-n 500]" shared by four commands.
func pathAndN(rest string, defN int) (string, int, error) {
	fields := strings.Fields(rest)
	path := ""
	n := defN
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-n" && i+1 < len(fields) {
			v, err := strconv.Atoi(fields[i+1])
			if err != nil || v <= 0 || v > 20000 {
				return "", 0, fmt.Errorf("-n wants 1..20000")
			}
			n = v
			i++
			continue
		}
		if path == "" {
			path = fields[i]
		}
	}
	if path == "" {
		return "", 0, fmt.Errorf("usage: <path> [-n %d]", defN)
	}
	return path, n, nil
}

// HandleText dispatches the five commands; returns markdown text.
func (g *LogAnalysisGate) HandleText(ctx context.Context, text string) string {
	cmd := strings.TrimSpace(strings.SplitN(text, " ", 2)[0])
	rest := ""
	if i := strings.Index(text, " "); i >= 0 {
		rest = strings.TrimSpace(text[i+1:])
	}
	switch cmd {
	case "/api_perf":
		path, n, err := pathAndN(rest, 500)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if s, done := g.analyzeViaTool(ctx, "perf", path, n); done {
			return s
		}
		r, err := loganalysis.AnalyzeAPIPerf(path, n)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		return fmt.Sprintf("%s *API performance* — `%s`\n%d requests · p50 *%.0fms* · p95 *%.0fms* · slow >1s: %d\n5xx: %d (%.1f%%)",
			perfEmoji(r.ErrPct, r.P95), r.Path, r.Requests, r.P50, r.P95, r.Slow, r.Errors, r.ErrPct) +
			kvBlock("Slowest endpoints", r.Slowest) +
			kvBlock("Status codes", r.StatusTop) +
			kvBlock("Top routes", r.PathTop)

	case "/exceptions":
		path, n, err := pathAndN(rest, 500)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if s, done := g.analyzeViaTool(ctx, "exceptions", path, n); done {
			return s
		}
		r, err := loganalysis.AnalyzeExceptions(path, n)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		head := fmt.Sprintf("🧨 *Exceptions* — `%s`\n%d/%d error-ish lines", r.Path, r.ErrorLines, r.LinesRead)
		return head + kvBlock("Templates", r.Top) +
			"\n\n*Last*\n`" + clamp(r.RecentLast, 120) + "`"

	case "/access":
		path, n, err := pathAndN(rest, 500)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if s, done := g.analyzeViaTool(ctx, "access", path, n); done {
			return s
		}
		r, err := loganalysis.AnalyzeAccess(path, n)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		return fmt.Sprintf("🚪 *Access* — `%s`\n%d hits", r.Path, r.Hits) +
			kvBlock("Top IPs", r.TopIPs) +
			kvBlock("Top routes", r.TopPaths) +
			kvBlock("Status", r.TopStatus) +
			kvBlock("Agents", r.TopAgent)

	case "/audit":
		n := 200
		if rest != "" {
			_, parsed, err := pathAndN("/dev/null "+rest, 200)
			if err != nil {
				return "⚠️ usage: /audit [-n 200]"
			}
			n = parsed
		}
		rows, err := g.QueryAuditRows(ctx, n)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if len(rows) == 0 {
			return "📭 audit trail kosong"
		}
		s := loganalysis.AnalyzeAuditRows(rows)
		return fmt.Sprintf("🧾 *Audit trail* (last %d)\nerrors %d (%.1f%%) · conf avg *%.0f* · low<60: %d · p50 %.0fms · p95 %.0fms",
			s.Total, s.ErrRows, s.ErrPct, s.ConfAvg, s.ConfLow, s.P50, s.P95) +
			kvBlock("Decision source", s.BySource) +
			kvBlock("Top rules", s.TopRules)

	case "/behaviour":
		path, n, err := pathAndN(rest, 1000)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if s, done := g.analyzeViaTool(ctx, "behaviour", path, n); done {
			return s
		}
		r, err := loganalysis.AnalyzeBehaviour(path, n)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		return fmt.Sprintf("🔁 *Behaviour* — `%s`\n%d events · novel shapes: %d", r.Path, r.Actions, r.NewShapes) +
			kvBlock("Top actions", r.TopCmds) +
			kvBlock("Peak hours", r.HourHist) +
			kvBlock("Bursts (per minute)", r.Bursts)
	}
	return "unknown analysis command: " + cmd
}

func perfEmoji(errPct, p95 float64) string {
	switch {
	case errPct > 5 || p95 > 3000:
		return "🔴"
	case errPct > 1 || p95 > 1000:
		return "🟡"
	default:
		return "🟢"
	}
}

// kvBlock renders "*Title*\n• `N×` key..." or "" when empty.
func kvBlock(title string, kvs []loganalysis.KV) string {
	if len(kvs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n*%s*\n", title)
	for _, kv := range kvs {
		fmt.Fprintf(&b, "• `%d×` %s\n", kv.Count, kv.Key)
	}
	return b.String()
}
