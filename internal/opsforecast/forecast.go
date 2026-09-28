// Package opsforecast — predictive layer over the same log families
// loganalysis already reads (owner directive 28 Sep: AegisGo evolves from
// reactive log watcher into an AI-Ops predictive helper). Everything here
// is deterministic: time-bucketing + least-squares slope + EWMA. No LLM,
// no network, bounded reads — the AegisGo way.
//
// The predictor answers one question well: "given the recent trend, when
// does this metric cross a threshold, and how confident are we?" — the
// output is designed to feed a recommended_action envelope later (scaling
// hooks stay outside v1: recommend first, automate later).
package opsforecast

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bucket is one time-window of aggregated log activity.
type Bucket struct {
	Start time.Time
	Count int     // requests
	Errs  int     // status >= 500
	P95   float64 // p95 latency within the bucket (0 if none parsed)

	lat []float64 // unexported: raw latencies for exact bucket p95
}

// Metric identifies what is being forecast.
type Metric string

const (
	MetricReqRate Metric = "req_rate"    // requests per bucket
	MetricErrRate Metric = "err_rate"    // errors per bucket (raw count)
	MetricLatency Metric = "latency_p95" // bucket p95, ms
)

// Forecast is the deterministic prediction over a metric's series.
type Forecast struct {
	Metric   Metric
	Buckets  int
	Window   time.Duration
	First    time.Time
	Slope    float64   // units per bucket (least squares over all buckets)
	EWMA     float64   // smoothed last value (alpha 0.4)
	LastVal  float64   // raw last-bucket value
	LastTS   time.Time // start of the last bucket
	UnitsPer float64   // per-minute rate derived from slope/window
	Level    Level     // trend interpretation
	// ETA: buckets until the projected value crosses Threshold, only set
	// when the trend actually reaches it within 24h and confidence holds.
	Threshold  float64
	ETABuckets float64
	ETAAt      time.Time
	HitIn24h   bool
	// Confidence 0-100: bucket count, dispersion around the fit, and
	// whether the last bucket agrees with the trend direction.
	Confidence int
}

// Level is the human summary of the slope.
type Level string

const (
	LevelStable   Level = "stable"
	LevelRising   Level = "rising"
	LevelFalling  Level = "falling"
	LevelVolatile Level = "volatile"
)

// timestamp shapes we understand, tried in order. Go log default first
// ([date time]), then common structured forms.
var (
	reTsGoLog   = regexp.MustCompile(`^\[(\d{4}/\d{2}/\d{2}) (\d{2}:\d{2}:\d{2})`)
	reTsISO     = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})`)
	reTsEpoch   = regexp.MustCompile(`\b(\d{10})\b`) // unix seconds (also inside ms)
	reTsEpochMs = regexp.MustCompile(`\b(\d{13})\b`)
)

var reStatus = regexp.MustCompile(`(?:status=|"status":|HTTP/1\.[01]" )(\d{3})|(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+\S+\s+(\d{3})\b`)
var reLatMS = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s*(?:ms|milliseconds)\b`)
var reLatS = regexp.MustCompile(`\b(\d+(?:\.\d+)?)s\b`)

// parseClock extracts a wall-clock timestamp from a line, returning the
// zero time when nothing recognizable is present.
func parseClock(l string) (time.Time, bool) {
	if m := reTsGoLog.FindStringSubmatch(l); m != nil {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1]+" "+m[2], time.Local); err == nil {
			return t, true
		}
	}
	if m := reTsISO.FindStringSubmatch(l); m != nil {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1]+" "+m[2], time.Local); err == nil {
			return t, true
		}
	}
	if m := reTsEpochMs.FindStringSubmatch(l); m != nil {
		ms, _ := strconv.ParseInt(m[1], 10, 64)
		return time.UnixMilli(ms), true
	}
	if m := reTsEpoch.FindStringSubmatch(l); m != nil {
		s, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(s, 0), true
	}
	return time.Time{}, false
}

// latencyMS parses an inline latency, same semantics as loganalysis
// (seconds candidates are vetted so "5 status" is not 5s).
func latencyMS(l string) float64 {
	if m := reLatMS.FindStringSubmatch(l); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	if m := reLatS.FindStringSubmatch(l); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v * 1000
	}
	return -1
}

// statusCodeOf pulls the HTTP status from a line using reStatus; the
// pattern has two capture shapes (named forms vs bare "METHOD path NNN").
func statusCodeOf(l string) int {
	m := reStatus.FindStringSubmatch(l)
	if m == nil {
		return 0
	}
	if m[1] != "" {
		c, _ := strconv.Atoi(m[1])
		return c
	}
	c, _ := strconv.Atoi(m[2])
	return c
}

// BuildBuckets aggregates the last n lines into fixed windows. Lines
// without a parseable timestamp are skipped (and counted) — a forecast
// over guessed clocks is worse than no forecast.
func BuildBuckets(path string, n int, window time.Duration) ([]Bucket, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	const maxWindow8MiB = 8 << 20
	off := int64(0)
	if info.Size() > maxWindow8MiB {
		off = info.Size() - maxWindow8MiB
	}
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, 0, err
	}
	lines := strings.Split(string(buf), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	bySlot := map[int64]*Bucket{}
	skipped := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ts, ok := parseClock(l)
		if !ok {
			skipped++
			continue
		}
		slot := ts.Truncate(window).Unix()
		b := bySlot[slot]
		if b == nil {
			b = &Bucket{Start: ts.Truncate(window)}
			bySlot[slot] = b
		}
		b.Count++
		if code := statusCodeOf(l); code >= 500 {
			b.Errs++
		}
		if lat := latencyMS(l); lat >= 0 {
			b.lat = append(b.lat, lat)
		}
	}
	out := make([]Bucket, 0, len(bySlot))
	for _, b := range bySlot {
		if len(b.lat) > 0 {
			sort.Float64s(b.lat)
			idx := int(math.Ceil(float64(len(b.lat)-1) * 0.95))
			if idx < 0 {
				idx = 0
			}
			b.P95 = b.lat[idx]
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, skipped, nil
}

// leastSquares returns slope and r² goodness of fit for y over x=0..n-1.
func leastSquares(y []float64) (slope, r2 float64) {
	n := float64(len(y))
	if len(y) < 2 {
		return 0, 0
	}
	var sx, sy, sxx, sxy float64
	for i, v := range y {
		sx += float64(i)
		sy += v
		sxx += float64(i) * float64(i)
		sxy += float64(i) * v
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, 0
	}
	slope = (n*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / n
	// r²: 1 - SSres/SStot
	var ssr, sst float64
	mean := sy / n
	for i, v := range y {
		pred := slope*float64(i) + intercept
		ssr += (v - pred) * (v - pred)
		sst += (v - mean) * (v - mean)
	}
	if sst == 0 {
		r2 = 1 // flat line fits perfectly
	} else {
		r2 = 1 - ssr/sst
	}
	return slope, r2
}

// ewma smooths the series with alpha; seeds with the first value.
func ewma(y []float64, alpha float64) float64 {
	if len(y) == 0 {
		return 0
	}
	e := y[0]
	for _, v := range y[1:] {
		e = alpha*v + (1-alpha)*e
	}
	return e
}

// ForecastMetric projects a metric series and answers when it crosses
// threshold. Confidence bands are empirical: ≥8 buckets and r² ≥ 0.35
// with last-value agreement → high; fewer buckets or noisy fit → low.
func ForecastMetric(m Metric, buckets []Bucket, window time.Duration, threshold float64) (*Forecast, error) {
	if len(buckets) < 3 {
		return nil, fmt.Errorf("need ≥3 time buckets for a trend, got %d (check log timestamps)", len(buckets))
	}
	y := make([]float64, len(buckets))
	for i, b := range buckets {
		switch m {
		case MetricReqRate:
			y[i] = float64(b.Count)
		case MetricErrRate:
			y[i] = float64(b.Errs)
		case MetricLatency:
			y[i] = b.P95
		}
	}
	slope, r2 := leastSquares(y)
	e := ewma(y, 0.4)
	fc := &Forecast{
		Metric:    m,
		Buckets:   len(buckets),
		Window:    window,
		First:     buckets[0].Start,
		Slope:     slope,
		EWMA:      e,
		LastVal:   y[len(y)-1],
		LastTS:    buckets[len(buckets)-1].Start,
		UnitsPer:  slope / window.Minutes(),
		Threshold: threshold,
	}

	// level
	abs := math.Abs(slope)
	scale := math.Max(math.Abs(e), 1) // relative to the metric's own size
	switch {
	case abs/scale < 0.02:
		fc.Level = LevelStable
	case slope > 0:
		fc.Level = LevelRising
	default:
		fc.Level = LevelFalling
	}
	if r2 < 0.1 && abs/scale >= 0.02 {
		fc.Level = LevelVolatile
	}

	// confidence
	conf := 0
	if len(buckets) >= 8 {
		conf += 30
	} else {
		conf += len(buckets) * 3
	}
	conf += int(math.Max(0, r2) * 50) // r² 0→0, 1→50
	lastAgrees := (y[len(y)-1]-y[0])*slope >= 0
	if lastAgrees {
		conf += 20
	}
	fc.Confidence = clampInt(conf, 0, 100)

	// ETA to threshold (linear projection from the EWMA level)
	cur := e
	switch {
	case slope > 0 && threshold > cur:
		need := (threshold - cur) / slope
		fc.ETABuckets = need
		fc.ETAAt = fc.LastTS.Add(time.Duration(need * float64(window)))
		fc.HitIn24h = time.Duration(need*float64(window)) <= 24*time.Hour
	case cur >= threshold:
		// already at/over threshold regardless of slope direction
		fc.ETABuckets = 0
		fc.ETAAt = fc.LastTS
		fc.HitIn24h = true
	}
	return fc, nil
}

// String renders the Telegram-friendly one-liner.
func (f *Forecast) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d buckets (%s window), last %.1f, ewma %.1f, slope %+.2f/bucket (r²-conf %d%%)",
		f.Metric, f.Level, f.Buckets, f.Window, f.LastVal, f.EWMA, f.Slope, f.Confidence)
	if f.ETABuckets >= 0 && f.Threshold > 0 {
		if f.ETABuckets == 0 {
			fmt.Fprintf(&b, " — ALREADY at/over threshold %.1f", f.Threshold)
		} else if f.HitIn24h {
			fmt.Fprintf(&b, " — crosses %.1f in ~%.0f buckets (~%s)",
				f.Threshold, f.ETABuckets, f.ETAAt.Sub(f.LastTS).Round(time.Minute))
		} else {
			fmt.Fprintf(&b, " — threshold %.1f beyond 24h horizon", f.Threshold)
		}
	}
	return b.String()
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
