// Package logwatch tails a log file 24/7 and fires alerts when a pattern
// hits (owner directive 28 Sep: "read logs file and find pattern to
// monitor it 24 hours by spawning go routines, like watchdog").
//
// Design mirrors what the Hermes run-ledger taught us: a monitor that
// alerts on EVERY match is noise; one that dedupes and re-arms is signal.
// Each watch is one goroutine via loop.Periodic (the repo's one tested
// loop shape — CLAUDE.md: no hand-rolled tickers) polling with stat+tail,
// not inotify: log rotation shows up as a size drop and is handled, and
// polling keeps the whole thing testable with tiny intervals.
package logwatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aegisgo/internal/loop"
)

// Alert is one fired notification: the watch name, the matching line
// (trimmed, capped), and when. Consumers (Telegram notifier, audit log)
// format it themselves.
type Alert struct {
	Watch string
	Line  string
	At    time.Time
}

// Watch is one pattern over one file.
type Watch struct {
	Name    string        // stable id, e.g. "gateway-errors"
	Path    string        // log file path
	Pattern string        // regex, matched case-insensitively line-wise
	Every   time.Duration // poll interval; <=0 means 30s
	// Cooldown suppresses repeat alerts for the same watch this long
	// (default 10m; 0 disables suppression only if explicitly set negative).
	Cooldown time.Duration
}

// Manager owns the watch goroutines. All methods safe for concurrent use.
type Manager struct {
	mu      sync.Mutex
	watches map[string]*running
	alerts  chan Alert
	logger  interface{ Info(string, ...any) }
}

type running struct {
	spec          Watch
	stop          func()
	lastLine      int64 // offset already read
	lastSize      int64 // size at last poll (rotation detector)
	cooldownUntil time.Time
	matches       int // total lines matched (counted even when suppressed)
}

// NewManager builds a manager. alertBuffer bounds the alert channel; when
// full, alerts are dropped (a flooded chat is worse than a dropped line).
func NewManager(alertBuffer int) *Manager {
	if alertBuffer <= 0 {
		alertBuffer = 16
	}
	return &Manager{
		watches: make(map[string]*running),
		alerts:  make(chan Alert, alertBuffer),
	}
}

// Alerts exposes the alert stream (read until Manager.Stop).
func (m *Manager) Alerts() <-chan Alert { return m.alerts }

// Add registers and starts a watch. A duplicate name replaces the old one
// (stop + re-add), so config reloads stay simple. The pattern must
// compile; a bad one is rejected at Add time, never mid-loop.
func (m *Manager) Add(ctx context.Context, w Watch) error {
	if w.Name == "" || w.Path == "" || w.Pattern == "" {
		return fmt.Errorf("watch needs name, path, and pattern")
	}
	re, err := regexp.Compile("(?i)" + w.Pattern)
	if err != nil {
		return fmt.Errorf("watch %q pattern: %v", w.Name, err)
	}
	if w.Every <= 0 {
		w.Every = 30 * time.Second
	}
	if w.Cooldown == 0 {
		w.Cooldown = 10 * time.Minute
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.watches[w.Name]; ok {
		old.stop()
		delete(m.watches, w.Name)
	}
	r := &running{spec: w}
	poll := func(pctx context.Context) {
		lines, newSize, newOff := tailFile(w.Path, r.lastSize, r.lastLine)
		r.lastSize, r.lastLine = newSize, newOff
		for _, ln := range lines {
			if re.MatchString(ln) {
				r.matches++
				now := time.Now()
				if now.Before(r.cooldownUntil) {
					continue // suppressed: counted, not notified
				}
				if w.Cooldown > 0 {
					r.cooldownUntil = now.Add(w.Cooldown)
				}
				select {
				case m.alerts <- Alert{Watch: w.Name, Line: capLine(ln), At: now}:
				default: // buffer full: drop, flooding helps nobody
				}
			}
		}
	}
	// First poll immediately so an existing bad line is seen at startup.
	poll(ctx)
	r.stop = loop.Periodic(ctx, w.Every, 10*time.Second, poll)
	m.watches[w.Name] = r
	return nil
}

// Stop tears down every watch (idempotent).
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, r := range m.watches {
		r.stop()
		delete(m.watches, name)
	}
}

// Counts returns matches per watch (for /status).
func (m *Manager) Counts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.watches))
	for name, r := range m.watches {
		out[name] = r.matches
	}
	return out
}

// tailFile reads the NEW content of path since offset. Rotation or
// truncation (size < offset) resets to the head. Missing file = no lines,
// no error: logs appear later.
func tailFile(path string, lastSize, lastOff int64) (lines []string, size, off int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, lastSize, lastOff
	}
	// Defense in depth against the parse-time extension allowlist: a
	// non-regular file (renamed device, pipe, socket) must not reach the
	// size-based buffer allocation below — /dev/zero renamed to x.log
	// would otherwise read forever and blow RAM.
	if !fi.Mode().IsRegular() {
		return nil, lastSize, lastOff
	}
	size = fi.Size()
	if size < lastOff || size < lastSize {
		lastOff = 0 // rotated/truncated: read from head
	}
	if size == lastOff {
		return nil, size, lastOff // nothing new
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, size, lastOff
	}
	defer f.Close()
	buf := make([]byte, size-lastOff)
	n, err := f.ReadAt(buf, lastOff)
	if err != nil && n == 0 {
		return nil, size, lastOff
	}
	for _, ln := range strings.Split(string(buf[:n]), "\n") {
		if t := strings.TrimRight(ln, "\r"); t != "" {
			lines = append(lines, t)
		}
	}
	return lines, size, lastOff + int64(n)
}

// capLine bounds an alert line so one giant log line can't blow a
// Telegram message limit.
func capLine(s string) string {
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Remove stops and unregisters a watch by name; true if it existed.
func (m *Manager) Remove(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.watches[name]
	if !ok {
		return false
	}
	r.stop()
	delete(m.watches, name)
	return true
}

// Specs returns the live watch definitions (for /watchlist and boot
// re-hydration).
func (m *Manager) Specs() []Watch {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Watch, 0, len(m.watches))
	for _, r := range m.watches {
		out = append(out, r.spec)
	}
	return out
}

// Analysis is a deterministic pattern report over a log tail: what the
// log repeats, how much of it is errors, and which error shapes dominate.
// "Menganalisa logs untuk pattern" (owner 28 Sep) runs as pure code —
// no LLM needed to see that 62% of the tail is the same timeout.
type Analysis struct {
	Path       string       `json:"path"`
	LinesRead  int          `json:"lines_read"`
	ErrorLines int          `json:"error_lines"`
	ErrorRate  float64      `json:"error_rate"`
	Top        []PatternHit `json:"top_patterns"`
	TopErrors  []PatternHit `json:"top_error_patterns"`
}

// PatternHit is one normalized line shape: template with the volatile
// parts masked, how often, and one raw sample.
type PatternHit struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
	Sample   string `json:"sample"`
}

var (
	// reVolatileKey masks key=value pairs whose value is inherently
	// unique per line (trace ids, sessions, sequence numbers) — the key
	// is kept, the value collapses so repeats group under one template.
	reVolatileKey = regexp.MustCompile(`(?i)((?:trace|request|session|span|parent)(?:[-_]?id)?|pid|tid|seq|ts)=\S+`)
	reDigits      = regexp.MustCompile(`\d+`)
	reHex         = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	reUUID        = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reIP          = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	reQuoted      = regexp.MustCompile(`"[^"]*"`)
	reDur         = regexp.MustCompile(`\d+(?:\.\d+)?(?:ms|µs|ns|s)\b`)
	reBrackets    = regexp.MustCompile(`\[[^\]]*\]`)
	reErrorish    = regexp.MustCompile(`(?i)\b(error|fatal|panic|warn(?:ing)?)\b`)
)

// normalize masks the volatile parts of a line so repeats collapse into
// one template: UUID, 0xHEX, IP, DUR, "STR", [...], digits → N.
func normalize(s string) string {
	// UUID first: trace_id=<uuid> should mask the VALUE but keep the key
	// readable — masking the key first would hide the uuid from its own
	// pass and double-mask digits into garbage.
	s = reUUID.ReplaceAllString(s, "UUID")
	s = reVolatileKey.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.IndexByte(m, '=')
		return m[:i+1] + "N"
	})
	s = reHex.ReplaceAllString(s, "0xHEX")
	s = reIP.ReplaceAllString(s, "IP")
	s = reDur.ReplaceAllString(s, "DUR")
	s = reQuoted.ReplaceAllString(s, `"STR"`)
	s = reBrackets.ReplaceAllString(s, "[...]")
	s = reDigits.ReplaceAllString(s, "N")
	return s
}

// Analyze reads the last `lines` lines of path (default 500, capped 5000)
// and reports pattern statistics. Deterministic, read-only, bounded: the
// read is a tail window (8 MiB max, Hard Rule 10 — never slurp a GB log)
// and non-regular files (devices, pipes, sockets renamed to *.log) are
// refused outright so /dev/zero can never feed the line splitter.
func Analyze(path string, lines int) (*Analysis, error) {
	if lines <= 0 {
		lines = 500
	}
	if lines > 5000 {
		lines = 5000
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	const maxWindow = 8 << 20 // 8 MiB tail window is plenty for 5000 lines
	off := int64(0)
	if fi.Size() > maxWindow {
		off = fi.Size() - maxWindow
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	all := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	a := &Analysis{Path: path, LinesRead: len(all)}
	tally := map[string]*PatternHit{}
	errTally := map[string]*PatternHit{}
	for _, ln := range all {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		t := normalize(ln)
		h := tally[t]
		if h == nil {
			h = &PatternHit{Template: t, Sample: capLine(ln)}
			tally[t] = h
		}
		h.Count++
		if reErrorish.MatchString(ln) {
			a.ErrorLines++
			eh := errTally[t]
			if eh == nil {
				eh = &PatternHit{Template: t, Sample: capLine(ln)}
				errTally[t] = eh
			}
			eh.Count++
		}
	}
	if a.LinesRead > 0 {
		a.ErrorRate = float64(a.ErrorLines) / float64(a.LinesRead)
	}
	a.Top = topN(tally, 5)
	a.TopErrors = topN(errTally, 5)
	return a, nil
}

func topN(m map[string]*PatternHit, n int) []PatternHit {
	out := make([]PatternHit, 0, len(m))
	for _, h := range m {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
