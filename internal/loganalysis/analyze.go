// Package loganalysis — deterministic, LLM-free analysis for the log
// families an ops bot actually gets asked about (owner directive 28 Sep):
// API performance, application exceptions, access logs, audit trails,
// and behaviour (per-actor/per-endpoint activity patterns).
//
// Everything here is pure computation over lines or rows — no model
// calls, no network. Output structs render to Telegram markdown via the
// app layer.
package loganalysis

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---- shared plumbing -------------------------------------------------

// readLastN streams a file backwards cheaply: full read only if small,
// else tail via ReadFrom with offset (logs can be GBs).
func readLastN(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const maxWindow = 8 << 20 // 8 MiB tail window is plenty for N lines
	off := int64(0)
	if info.Size() > maxWindow {
		off = info.Size() - maxWindow
	}
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	lines := strings.Split(string(buf), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is likely cut mid-way
	}
	// drop empties BEFORE windowing so a trailing newline doesn't eat a slot
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept, nil
}

// topK is the shared tally helper (mode: keep it simple).
type counter map[string]int

func (c counter) add(k string) { c[k]++ }

func (c counter) top(k int) []KV {
	out := make([]KV, 0, len(c))
	for key, n := range c {
		out = append(out, KV{Key: key, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// KV is a count pair (rendered as `N× key`).
type KV struct {
	Key   string
	Count int
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

// ---- 1) API performance ----------------------------------------------

// APIPerf is the deterministic summary of an HTTP access/latency log.
type APIPerf struct {
	Path       string
	LinesRead  int
	Requests   int
	Errors     int // status >= 500
	ErrPct     float64
	P50, P95   float64 // ms
	Slow       int     // > 1s
	Slowest    []KV    // top endpoints by p95-ish (avg of matched latencies)
	StatusTop  []KV
	PathTop    []KV
	UnknownFmt int // lines with status but no latency parsed
}

var (
	// common log format + variations with latency fields
	reStatus = regexp.MustCompile(`\b(?:status=|"status":|HTTP/1\.[01]" )(\d{3})`)
	reLatMS  = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s*(?:ms|milliseconds)\b`)
	// RE2 has no lookahead: match "N s"/"N.Ns" candidates and vet the
	// following char manually in parseLatMS (guards "5 status" vs "1.5s").
	reLatS  = regexp.MustCompile(`\b(\d+(?:\.\d+)?)s\b`)
	rePath  = regexp.MustCompile(`(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(\S+)`)
	reRoute = regexp.MustCompile(`(?:path|route|uri|url)="([^"]+)"`)
)

// AnalyzeAPIPerf computes the perf summary from the last N lines.
func AnalyzeAPIPerf(path string, n int) (*APIPerf, error) {
	lines, err := readLastN(path, n)
	if err != nil {
		return nil, err
	}
	r := &APIPerf{Path: path, LinesRead: len(lines)}
	latAll := make([]float64, 0, len(lines))
	latByPath := map[string][]float64{}
	stCount, pathCount := counter{}, counter{}

	for _, l := range lines {
		if m := reStatus.FindStringSubmatch(l); m != nil {
			r.Requests++
			code, _ := strconv.Atoi(m[1])
			stCount.add(m[1])
			if code >= 500 {
				r.Errors++
			}
		}
		p := ""
		if m := rePath.FindStringSubmatch(l); m != nil {
			p = m[1]
		} else if m := reRoute.FindStringSubmatch(l); m != nil {
			p = m[1]
		}
		// normalize: /v1/answers/abc → /v1/answers/:id
		p = normalizeRoute(p)
		lat := parseLatMS(l)
		if p != "" {
			pathCount.add(p)
			latByPath[p] = append(latByPath[p], lat)
		}
		if parseLatMS(l) >= 0 {
			latAll = append(latAll, lat)
		}
	}
	r.ErrPct = pct(r.Errors, r.Requests)
	r.StatusTop = stCount.top(5)
	r.PathTop = pathCount.top(8)

	if len(latAll) > 0 {
		sort.Float64s(latAll)
		r.P50 = percentile(latAll, 50)
		r.P95 = percentile(latAll, 95)
		for _, v := range latAll {
			if v > 1000 {
				r.Slow++
			}
		}
		// slowest endpoints by mean of their latencies
		type pa struct {
			k string
			v float64
		}
		var pas []pa
		for k, vs := range latByPath {
			sum := 0.0
			for _, v := range vs {
				sum += v
			}
			if len(vs) > 0 {
				pas = append(pas, pa{k, sum / float64(len(vs))})
			}
		}
		sort.Slice(pas, func(i, j int) bool { return pas[i].v > pas[j].v })
		for i, p := range pas {
			if i >= 5 {
				break
			}
			r.Slowest = append(r.Slowest, KV{Key: fmt.Sprintf("%s ~%.0fms", p.k, p.v), Count: len(latByPath[p.k])})
		}
	}
	return r, nil
}

// normalizeRoute collapses volatile path segments (ids, hashes) so
// per-endpoint stats group correctly: /v1/answers/abc123 → /v1/answers/:id
func normalizeRoute(p string) string {
	if p == "" {
		return p
	}
	segs := strings.Split(p, "?")[0]
	parts := strings.Split(segs, "/")
	for i, s := range parts {
		if s == "" {
			continue
		}
		if isVolatileSeg(s) {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

func isVolatileSeg(s string) bool {
	if len(s) > 24 { // long → hash/id
		return true
	}
	allDigit := true
	hasHex := false
	hasDigit := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
			continue
		case (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			allDigit = false
			hasHex = true
			continue
		}
		return false // plain word segment (g, z, -…)
	}
	// "123" pure number, "abc123"/"deadbeef42" hex+digit mixes, "abc" alone
	// stays a word: require a digit present when hex letters exist.
	if allDigit {
		return true
	}
	return hasHex && hasDigit
}

// parseLatMS extracts latency in ms; -1 when absent.
func parseLatMS(l string) float64 {
	if m := reLatMS.FindStringSubmatch(l); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	if m := reLatS.FindStringSubmatchIndex(l); m != nil {
		end := m[1]
		if end >= len(l) || l[end] == ' ' || l[end] == '\t' || l[end] == '\n' {
			v, _ := strconv.ParseFloat(l[m[2]:m[3]], 64)
			return v * 1000
		}
	}
	return -1
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	return sorted[idx]
}

// ---- 2) Exceptions ----------------------------------------------------

// ExcSummary groups error/exception/panic lines by template.
type ExcSummary struct {
	Path        string
	LinesRead   int
	ErrorLines  int
	Top         []KV // templates
	RecentFirst string
	RecentLast  string
}

var reErrorish = regexp.MustCompile(`(?i)\b(error|exception|panic|fatal|critical|warn(?:ing)?)\b`)

// AnalyzeExceptions groups error-ish lines by normalized template.
func AnalyzeExceptions(path string, n int) (*ExcSummary, error) {
	lines, err := readLastN(path, n)
	if err != nil {
		return nil, err
	}
	e := &ExcSummary{Path: path, LinesRead: len(lines)}
	tpl := counter{}
	// reuse logwatch-style normalization without importing it (keep
	// packages independent: this one is row/line-generic)
	for _, l := range lines {
		if !reErrorish.MatchString(l) {
			continue
		}
		e.ErrorLines++
		if e.RecentFirst == "" {
			e.RecentFirst = l
		}
		e.RecentLast = l
		tpl.add(Normalize(l))
	}
	e.Top = tpl.top(8)
	return e, nil
}

// Normalize masks volatile details so repeated errors collapse to one
// shape (shared contract with logwatch — tested identical in spirit).
var (
	nUUID  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	nIP    = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	nDUR   = regexp.MustCompile(`\b\d+(?:\.\d+)?\s*(?:ms|us|µs|ns|s)\b`)
	nSTR   = regexp.MustCompile(`"[^"]*"`)
	nBrack = regexp.MustCompile(`\[[^\]]*\]`)
	nNum   = regexp.MustCompile(`\d+`)
	nVola  = regexp.MustCompile(`(?i)((?:trace|request|session|span|parent)(?:[-_]?id)?|pid|tid|seq|ts)=\S+`)
)

// Normalize returns the template of a line.
func Normalize(s string) string {
	s = nUUID.ReplaceAllString(s, "UUID")
	s = nVola.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.IndexByte(m, '=')
		return m[:i+1] + "N"
	})
	s = nIP.ReplaceAllString(s, "IP")
	s = nDUR.ReplaceAllString(s, "DUR")
	s = nSTR.ReplaceAllString(s, `"STR"`)
	s = nBrack.ReplaceAllString(s, "[...]")
	s = nNum.ReplaceAllString(s, "N")
	return s
}

// ---- 3) Access log -----------------------------------------------------

// AccessSummary: traffic shape — top IPs, paths, methods, status codes.
type AccessSummary struct {
	Path      string
	LinesRead int
	Hits      int
	TopIPs    []KV
	TopPaths  []KV
	TopStatus []KV
	TopAgent  []KV
}

var (
	reIP      = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3})`)
	reMethod  = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\b`)
	reAgent   = regexp.MustCompile(`"(?:Mozilla|curl|python|Go|okhttp|axios|PostmanRuntime)[^"]*"`)
	reAStatus = regexp.MustCompile(`\b(\d{3})\b`)
)

// AnalyzeAccess summarizes access-log shape.
func AnalyzeAccess(path string, n int) (*AccessSummary, error) {
	lines, err := readLastN(path, n)
	if err != nil {
		return nil, err
	}
	a := &AccessSummary{Path: path, LinesRead: len(lines)}
	ips, paths, sts, agents := counter{}, counter{}, counter{}, counter{}
	for _, l := range lines {
		a.Hits++
		if m := reIP.FindStringSubmatch(l); m != nil {
			ips.add(m[1])
		}
		if m := rePath.FindStringSubmatch(l); m != nil {
			paths.add(normalizeRoute(m[1]))
		}
		if m := reAgent.FindString(l); m != "" {
			agents.add(clampS(m, 40))
		}
		if m := reAStatus.FindStringSubmatch(l); m != nil {
			// prefer status near a method/HTTP marker to avoid matching ports
			if reStatus.FindStringSubmatch(l) != nil {
				sts.add(reStatus.FindStringSubmatch(l)[1])
			} else if isPlausibleStatus(m[1]) {
				sts.add(m[1])
			}
		}
	}
	a.TopIPs = ips.top(5)
	a.TopPaths = paths.top(8)
	a.TopStatus = sts.top(6)
	a.TopAgent = agents.top(4)
	return a, nil
}

func isPlausibleStatus(s string) bool {
	return strings.HasPrefix(s, "2") || strings.HasPrefix(s, "3") ||
		strings.HasPrefix(s, "4") || strings.HasPrefix(s, "5")
}

func clampS(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- 4) Audit trail (AegisGo's own audit_events) -----------------------

// AuditSummary is a rollup of the agent's own decision audit table.
type AuditSummary struct {
	Total    int
	BySource []KV // decision_source counts
	ErrRows  int
	ErrPct   float64
	P50, P95 float64 // latency ms
	ConfLow  int     // confidence < 60
	ConfAvg  float64
	Latest   string // latest row stamp (from row map if available)
	TopRules []KV
}

// AnalyzeAuditRows computes the summary over raw audit rows (map per
// row — the store adapter reuses QueryMaps; keys: interface,
// decision_source, rule_id, outcome, latency_ms, confidence, tokens_in,
// tokens_out).
func AnalyzeAuditRows(rows []map[string]any) *AuditSummary {
	s := &AuditSummary{}
	src, rules := counter{}, counter{}
	lat := make([]float64, 0, len(rows))
	var confSum float64
	var confN int
	for _, r := range rows {
		s.Total++
		src.add(str(r["decision_source"]))
		if str(r["rule_id"]) != "" {
			rules.add(str(r["rule_id"]))
		}
		if str(r["outcome"]) == "error" {
			s.ErrRows++
		}
		if v := fnum(r["latency_ms"]); v >= 0 {
			lat = append(lat, v)
		}
		if c := fnum(r["confidence"]); c >= 0 {
			confSum += c
			confN++
			if c < 60 {
				s.ConfLow++
			}
		}
	}
	s.ErrPct = pct(s.ErrRows, s.Total)
	s.BySource = src.top(6)
	s.TopRules = rules.top(6)
	if len(lat) > 0 {
		sort.Float64s(lat)
		s.P50 = percentile(lat, 50)
		s.P95 = percentile(lat, 95)
	}
	if confN > 0 {
		s.ConfAvg = confSum / float64(confN)
	}
	return s
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func fnum(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	}
	return -1
}

// ---- 5) Behaviour ------------------------------------------------------

// BehaviourSummary: WHAT the actor(s) do repeatedly — command/endpoint
// cadence, burst detection, new-vs-known shapes, hourly histogram.
type BehaviourSummary struct {
	Path      string
	LinesRead int
	Actions   int
	TopCmds   []KV
	HourHist  []KV // "09" → count (peak first 5)
	Bursts    []KV // "actor:minute" with > threshold hits
	NewShapes int  // templates seen once in window (novel behaviour)
	TopShapes []KV
}

var (
	reCmd    = regexp.MustCompile(`(?i)\b(?:cmd|command|action|event)=("?)([a-z_.:/-]+)"?`)
	reTGCmd  = regexp.MustCompile(`(?i)(/\w+)`)
	reHour   = regexp.MustCompile(`\b(\d{2}):(\d{2}):(\d{2})`)
	reISO    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ](\d{2}):\d{2}`)
	reMinute = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2})`)
)

// AnalyzeBehaviour finds repeated actor behaviour in any event-ish log.
func AnalyzeBehaviour(path string, n int) (*BehaviourSummary, error) {
	lines, err := readLastN(path, n)
	if err != nil {
		return nil, err
	}
	b := &BehaviourSummary{Path: path, LinesRead: len(lines)}
	cmds, hours, bursts, shapes := counter{}, counter{}, counter{}, counter{}
	for _, l := range lines {
		b.Actions++
		if m := reCmd.FindStringSubmatch(l); m != nil {
			cmds.add(m[2])
		} else if m := reTGCmd.FindStringSubmatch(l); m != nil {
			cmds.add(m[1])
		}
		h := ""
		if m := reISO.FindStringSubmatch(l); m != nil {
			h = m[1]
		} else if m := reHour.FindStringSubmatch(l); m != nil {
			h = m[1]
		}
		if h != "" {
			hours.add(h)
		}
		// burst: same minute bucket (date hh:mm)
		if m := reMinute.FindStringSubmatch(l); m != nil {
			bursts.add(m[1])
		}
		shapes.add(Normalize(l))
	}
	b.TopCmds = cmds.top(8)
	b.HourHist = hours.top(24)
	b.Bursts = bursts.top(5)
	b.TopShapes = shapes.top(6)
	for _, kv := range shapes.top(1 << 30) {
		if kv.Count == 1 {
			b.NewShapes++
		}
	}
	return b, nil
}
