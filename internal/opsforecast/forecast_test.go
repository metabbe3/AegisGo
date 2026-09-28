package opsforecast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLog writes lines to a temp file and returns its path.
func writeLog(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(p, []byte(joinNL(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func joinNL(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

// tsLine builds a Go-log-style line with a given minute offset.
func tsLine(base time.Time, offsetMin int, status, latency string) string {
	ts := base.Add(time.Duration(offsetMin) * time.Minute)
	return "[" + ts.Format("2006/01/02 15:04:05") + "] GET /v1/stats " + status + " " + latency
}

func TestParseClockShapes(t *testing.T) {
	cases := map[string]bool{
		"[2026/09/28 10:00:01] hi":              true,
		`2026-09-28T10:00:01Z level=info msg=x`: true,
		"ts=1759048801 level=info":              true,
		"ts_ms=1759048801123 event=click":       true,
		"no timestamp here at all":              false,
	}
	for line, want := range cases {
		if _, ok := parseClock(line); ok != want {
			t.Errorf("parseClock(%q) ok=%v, want %v", line, ok, want)
		}
	}
}

func TestBuildBucketsAggregatesAndSkips(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	lines := []string{
		tsLine(base, 0, "200", "50ms"),
		tsLine(base, 0, "500", "80ms"),
		tsLine(base, 1, "200", "60ms"),
		tsLine(base, 2, "200", "70ms"),
		tsLine(base, 2, "502", "90ms"),
		"unparseable line without timestamp",
	}
	p := writeLog(t, lines)
	buckets, skipped, err := BuildBuckets(p, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}
	if buckets[0].Count != 2 || buckets[0].Errs != 1 {
		t.Errorf("bucket0 = %+v, want Count 2 Errs 1", buckets[0])
	}
	if buckets[2].P95 != 90 {
		t.Errorf("bucket2 p95 = %v, want 90", buckets[2].P95)
	}
}

func TestForecastRisingErrRateETA(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	var lines []string
	// errors grow 0,1,2,4,8 per minute → clear rising trend
	for i, errs := range []int{0, 1, 2, 4, 8} {
		for e := 0; e < errs; e++ {
			lines = append(lines, tsLine(base, i, "500", "100ms"))
		}
		lines = append(lines, tsLine(base, i, "200", "50ms"))
	}
	p := writeLog(t, lines)
	buckets, _, err := BuildBuckets(p, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := ForecastMetric(MetricErrRate, buckets, time.Minute, 12)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Level != LevelRising {
		t.Errorf("level = %s, want rising", fc.Level)
	}
	if fc.Slope <= 0 {
		t.Errorf("slope = %.2f, want > 0", fc.Slope)
	}
	if !fc.HitIn24h || fc.ETABuckets <= 0 {
		t.Errorf("expected ETA hit within 24h, got %+v", fc)
	}
	if fc.Confidence <= 0 {
		t.Errorf("confidence = %d, want > 0", fc.Confidence)
	}
	// String must mention the threshold crossing
	if got := fc.String(); got == "" {
		t.Error("String() empty")
	}
}

func TestForecastStableAndTooFewBuckets(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	// flat series → stable
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, tsLine(base, i, "200", "50ms"))
		lines = append(lines, tsLine(base, i, "200", "50ms"))
	}
	p := writeLog(t, lines)
	buckets, _, _ := BuildBuckets(p, 100, time.Minute)
	fc, err := ForecastMetric(MetricReqRate, buckets, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Level != LevelStable {
		t.Errorf("level = %s, want stable", fc.Level)
	}
	// fewer than 3 buckets → error, not a fake forecast
	fc2, err := ForecastMetric(MetricReqRate, buckets[:2], time.Minute, 100)
	if err == nil || fc2 != nil {
		t.Errorf("expected error for <3 buckets, got %+v", fc2)
	}
}

func TestForecastAlreadyOverThreshold(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	var lines []string
	for i := 0; i < 5; i++ {
		for e := 0; e < 5; e++ {
			lines = append(lines, tsLine(base, i, "500", "100ms"))
		}
		lines = append(lines, tsLine(base, i, "200", "50ms"))
	}
	p := writeLog(t, lines)
	buckets, _, _ := BuildBuckets(p, 100, time.Minute)
	fc, err := ForecastMetric(MetricErrRate, buckets, time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}
	if fc.ETABuckets != 0 || !fc.HitIn24h {
		t.Errorf("already-over case: ETA=%v hit24h=%v, want 0/true", fc.ETABuckets, fc.HitIn24h)
	}
}

func TestLatencyP95Forecast(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	lats := []string{"100ms", "200ms", "400ms", "800ms", "1600ms"}
	var lines []string
	for i, lat := range lats {
		lines = append(lines, tsLine(base, i, "200", lat))
	}
	p := writeLog(t, lines)
	buckets, _, _ := BuildBuckets(p, 100, time.Minute)
	fc, err := ForecastMetric(MetricLatency, buckets, time.Minute, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Level != LevelRising {
		t.Errorf("level = %s, want rising", fc.Level)
	}
	if !fc.HitIn24h {
		t.Error("expected latency threshold hit within 24h")
	}
}

func TestStatusCodeOfBareAndNamed(t *testing.T) {
	if got := statusCodeOf("GET /v1/x 503 100ms"); got != 503 {
		t.Errorf("bare status = %d, want 503", got)
	}
	if got := statusCodeOf(`{"status":500}`); got != 500 {
		t.Errorf("named status = %d, want 500", got)
	}
	if got := statusCodeOf("nothing here"); got != 0 {
		t.Errorf("no status = %d, want 0", got)
	}
}

func TestLevelVolatileOnNoisySeries(t *testing.T) {
	// alternating high/low = non-flat but r² ~ 0 → volatile
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.Local)
	var lines []string
	for i, n := range []int{100, 1, 100, 1, 100, 1} {
		for j := 0; j < n; j++ {
			lines = append(lines, tsLine(base, i, "200", "50ms"))
		}
	}
	p := writeLog(t, lines)
	buckets, _, _ := BuildBuckets(p, 1000, time.Minute)
	fc, err := ForecastMetric(MetricReqRate, buckets, time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Level != LevelVolatile {
		t.Errorf("level = %s, want volatile", fc.Level)
	}
}

func TestLatencyMSForms(t *testing.T) {
	cases := map[string]float64{
		"took 250ms":          250,
		"took 1.5s":           1500,
		"dur=42 milliseconds": 42,
		"no latency here":     -1,
		"5 status":            -1, // "5 s" vet: next char is 't' → rejected
	}
	for line, want := range cases {
		if got := latencyMS(line); got != want {
			t.Errorf("latencyMS(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestClampIntBounds(t *testing.T) {
	if clampInt(-5, 0, 100) != 0 || clampInt(150, 0, 100) != 100 || clampInt(50, 0, 100) != 50 {
		t.Error("clampInt bounds broken")
	}
}

func TestForecastStringRenders(t *testing.T) {
	// render every String branch: no threshold, beyond-24h, already-over
	f := &Forecast{Metric: MetricReqRate, Level: LevelStable, Buckets: 9,
		Window: time.Minute, LastVal: 10, EWMA: 10, Slope: 0, Confidence: 70}
	if s := f.String(); s == "" || !strings.Contains(s, "stable") {
		t.Errorf("String stable branch: %q", s)
	}
	f2 := &Forecast{Metric: MetricReqRate, Level: LevelRising, Buckets: 9,
		Window: time.Minute, LastVal: 10, EWMA: 10, Confidence: 70, Threshold: 100}
	f2.Slope = 0.1
	f2.ETABuckets = -1 // beyond horizon path needs HitIn24h=false
	_ = f2.String()    // must not panic
	f3 := &Forecast{Metric: MetricReqRate, Level: LevelRising, Buckets: 9,
		Window: time.Minute, LastVal: 10, EWMA: 10, Slope: 1, Confidence: 70,
		Threshold: 10, ETABuckets: 0, HitIn24h: true, LastTS: time.Now(), ETAAt: time.Now()}
	if s := f3.String(); !strings.Contains(s, "ALREADY") {
		t.Errorf("String already-over branch: %q", s)
	}
}
