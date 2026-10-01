// Package app — watchgate.go: Telegram surface for the 24/7 log watchdog
// (owner directive 28 Sep): /watch /unwatch /watchlist /analyze, all
// runtime — no rebuild, definitions persist in SQLite (log_watches).
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"aegisgo/internal/logwatch"
)

// WatchGate wires the logwatch Manager (goroutines + alerts), the
// persistence adapter (SQLite), and the Telegram notifier (where alerts
// land). Add/remove is L1 (deterministic parsing + allowlisted effects)
// per the §L ladder: watching a file and pattern-matching are passive
// reads; only NOTIFICATION touches the outside world, to the owner chat.
type WatchGate struct {
	Mgr    *logwatch.Manager
	Store  *logwatch.StoreAdapter
	Notify func(text string) // owner-chat notifier; nil = alerts logged only
}

// checkWatchPath guards the watchdog's read surface. Unlike file tools,
// watches legitimately target logs OUTSIDE the workspace (/tmp/app.log),
// so full resolvePath containment would break the feature. Instead:
// kernel pseudo-filesystems (/dev /proc /sys) are rejected outright
// (block/char devices and synthetic files are not logs), and the target
// must carry a log-ish extension — a cheap allowlist that stops
// accidental watches on binaries, sockets, or dotfiles while keeping
// every real log path (/var/log, /tmp, project dirs) working.
func checkWatchPath(p string) error {
	for _, bad := range []string{"/dev/", "/proc/", "/sys/"} {
		if strings.HasPrefix(p, bad) || p == strings.TrimSuffix(bad, "/") {
			return fmt.Errorf("path %q is a kernel pseudo-filesystem, not a log file", p)
		}
	}
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".log", ".txt", ".out", ".err", ".ndjson", ".json":
		return nil
	}
	return fmt.Errorf("path %q must end in .log/.txt/.out/.err/.ndjson/.json", p)
}

// ParseWatch parses: /watch name=X | path=/a/b.log | pattern=panic|FATAL | every=30s | cooldown=10m
func ParseWatch(text string) (logwatch.StoredWatch, error) {
	w := logwatch.StoredWatch{}
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/watch"))
	if body == "" {
		return w, fmt.Errorf("usage: /watch name=X | path=/var/log/app.log | pattern=panic|FATAL | every=30s | cooldown=10m")
	}
	// Split on |, then re-attach pipe-less segments to the previous
	// value: a pattern like panic|FATAL must survive the pipe INSIDE the
	// regex. Merging left keeps the grammar one-pass and predictable.
	var segs []string
	for _, part := range strings.Split(body, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if len(segs) > 0 && !strings.Contains(part, "=") {
			segs[len(segs)-1] += "|" + part
			continue
		}
		segs = append(segs, part)
	}
	for _, seg := range segs {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) != 2 {
			return w, fmt.Errorf("bad segment %q (want key=value)", seg)
		}
		k, v := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1])
		switch k {
		case "name":
			w.Name = v
		case "path":
			w.Path = v
		case "pattern":
			w.Pattern = v
		case "every":
			d, err := parseDurLoose(v)
			if err != nil {
				return w, fmt.Errorf("every: %v", err)
			}
			w.Every = d
		case "cooldown":
			d, err := parseDurLoose(v)
			if err != nil {
				return w, fmt.Errorf("cooldown: %v", err)
			}
			w.Cooldown = d
		default:
			return w, fmt.Errorf("unknown key %q (want name|path|pattern|every|cooldown)", k)
		}
	}
	if w.Name == "" || w.Path == "" || w.Pattern == "" {
		return w, fmt.Errorf("name, path, and pattern are required")
	}
	if err := checkWatchPath(w.Path); err != nil {
		return w, err
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{1,40}$`).MatchString(w.Name) {
		return w, fmt.Errorf("name %q must be lowercase letters/digits/_/-", w.Name)
	}
	if _, err := regexp.Compile("(?i)" + w.Pattern); err != nil {
		return w, fmt.Errorf("pattern does not compile: %v", err)
	}
	if w.Every == 0 {
		w.Every = 30 * time.Second
	}
	if w.Cooldown == 0 {
		w.Cooldown = 10 * time.Minute
	}
	return w, nil
}

// parseDurLoose accepts 30s / 5m / 1h / plain-seconds.
func parseDurLoose(v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return d, nil
	}
	// bare number = seconds
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("bad duration %q (want 30s, 5m, 1h)", v)
}

// HandleWatchText is the dispatcher entry for /watch, /unwatch,
// /watchlist, /analyze. One function keeps the command family cohesive.
func (g *WatchGate) HandleWatchText(ctx context.Context, text string) string {
	cmd := strings.TrimSpace(strings.SplitN(text, " ", 2)[0])
	switch cmd {
	case "/watch":
		w, err := ParseWatch(text)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if err := g.Mgr.Add(ctx, logwatch.Watch{
			Name: w.Name, Path: w.Path, Pattern: w.Pattern,
			Every: w.Every, Cooldown: w.Cooldown,
		}); err != nil {
			return "⚠️ watch rejected: " + err.Error()
		}
		if err := g.Store.SaveWatch(ctx, w); err != nil {
			return "⚠️ live watch started but persisting failed: " + err.Error()
		}
		return fmt.Sprintf("✅ watch `%s` LIVE — `%s` matching `%s` (every %s, cooldown %s). Alerts → this chat.",
			w.Name, w.Path, w.Pattern, w.Every, w.Cooldown)

	case "/unwatch":
		name := strings.TrimSpace(strings.TrimPrefix(text, "/unwatch"))
		if name == "" {
			return "usage: /unwatch <name> (see /watchlist)"
		}
		g.Mgr.Remove(name)
		if err := g.Store.DeleteWatch(ctx, name); err != nil {
			return "⚠️ stopped live but delete failed: " + err.Error()
		}
		return "🗑 removed watch `" + name + "`"

	case "/watchlist":
		ws, err := g.Store.ListWatches(ctx)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if len(ws) == 0 {
			return "No watches yet. Add one:\n`/watch name=gw | path=/tmp/gw.log | pattern=panic|FATAL`"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "👁 *%d watch%s live*\n", len(ws), plural(len(ws)))
		for _, w := range ws {
			every := w.Every / time.Second
			fmt.Fprintf(&b, "\n• `%s` → `%s`\n  pattern `%s` · every %ds", w.Name, w.Path, w.Pattern, every)
		}
		counts := g.Mgr.Counts()
		if len(counts) > 0 {
			b.WriteString("\n\n_matches (since boot): ")
			first := true
			for n, c := range counts {
				if !first {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s=%d", n, c)
				first = false
			}
		}
		return b.String()

	case "/analyze":
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/analyze"))
		lines := 500
		parts := strings.Fields(rest)
		path := ""
		for i := 0; i < len(parts); i++ {
			if parts[i] == "-n" && i+1 < len(parts) {
				if _, err := fmt.Sscanf(parts[i+1], "%d", &lines); err != nil {
					return "⚠️ -n wants a number"
				}
				i++
				continue
			}
			if path == "" {
				path = parts[i]
			}
		}
		if path == "" {
			return "usage: /analyze <path> [-n 500]"
		}
		// Same parse-time guard as /watch: kernel pseudo-filesystems and
		// non-log extensions are refused before any file is opened.
		if err := checkWatchPath(path); err != nil {
			return "⚠️ " + err.Error()
		}
		a, err := logwatch.Analyze(path, lines)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		return FormatAnalysis(a)
	}
	return "unknown watch command: " + cmd
}

// FormatAnalysis renders an Analysis as Telegram-friendly markdown.
func FormatAnalysis(a *logwatch.Analysis) string {
	var b strings.Builder
	emoji := "🟢"
	switch {
	case a.ErrorRate > 0.3:
		emoji = "🔴"
	case a.ErrorRate > 0.1:
		emoji = "🟡"
	}
	fmt.Fprintf(&b, "%s *Log analysis* — `%s`\n", emoji, a.Path)
	fmt.Fprintf(&b, "%d lines · %d error-ish (%.1f%%)\n", a.LinesRead, a.ErrorLines, a.ErrorRate*100)
	if len(a.Top) > 0 {
		b.WriteString("\n*Top patterns*\n")
		for _, h := range a.Top {
			fmt.Fprintf(&b, "• `%d×` %s\n", h.Count, clamp(h.Template, 90))
		}
	}
	if len(a.TopErrors) > 0 {
		b.WriteString("\n*Top errors*\n")
		for _, h := range a.TopErrors {
			fmt.Fprintf(&b, "• `%d×` %s\n", h.Count, clamp(h.Template, 90))
		}
	} else {
		b.WriteString("\n✅ no error-ish lines in window")
	}
	return b.String()
}

func clamp(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// StartWatches rehydrates every stored watch at boot (persistence's whole
// point: restarts keep the watchdog set).
func (g *WatchGate) StartWatches(ctx context.Context) (int, error) {
	ws, err := g.Store.ListWatches(ctx)
	if err != nil {
		return 0, err
	}
	for _, w := range ws {
		if err := g.Mgr.Add(ctx, logwatch.Watch{
			Name: w.Name, Path: w.Path, Pattern: w.Pattern,
			Every: w.Every, Cooldown: w.Cooldown,
		}); err != nil {
			return 0, fmt.Errorf("rehydrating %q: %w", w.Name, err)
		}
	}
	if g.Notify != nil && len(ws) > 0 {
		// alert pump: manager alerts → owner chat
		alerts := g.Mgr.Alerts()
		go func() {
			for a := range alerts {
				g.Notify(fmt.Sprintf("🚨 *%s* matched:\n`%s`", a.Watch, a.Line))
			}
		}()
	}
	return len(ws), nil
}
