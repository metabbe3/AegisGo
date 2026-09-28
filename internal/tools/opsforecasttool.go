package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aegisgo/internal/opsforecast"
)

// OpsForecastToolInput is the schema for ops_forecast (tool #11).
type OpsForecastToolInput struct {
	// Path of the log file, relative to the workspace root.
	Path string `json:"path"`
	// Metric to forecast: "err_rate" (5xx per bucket), "req_rate"
	// (requests per bucket), "latency_p95" (bucket p95 ms).
	// Default "err_rate".
	Metric string `json:"metric,omitempty"`
	// Threshold the forecast watches for (e.g. 10 = alert when the
	// trend crosses 10 errors per bucket, 2000 = ms for latency).
	Threshold float64 `json:"threshold,omitempty"`
	// Bucket window in minutes. Default 1, max 60.
	WindowMin int `json:"window_min,omitempty"`
	// Lines window from the end of the file. Default 2000, max 20000.
	MaxLines int `json:"max_lines,omitempty"`
}

// OpsForecastToolOutput is the flat, LLM-friendly forecast summary.
type OpsForecastToolOutput struct {
	Metric    string `json:"metric"`
	Path      string `json:"path"`
	Buckets   int    `json:"buckets"`
	WindowMin int    `json:"window_min"`
	Skipped   int    `json:"skipped_no_ts"`
	Summary   string `json:"summary"`
	// RecommendedAction is the deterministic ops advice derived from the
	// forecast (owner directive: AegisGo as AI-ops predictive helper —
	// recommend first, automate later). Empty when there is nothing to do.
	RecommendedAction string `json:"recommended_action,omitempty"`
}

// metricOf maps the public name to the internal constant.
func metricOf(s string) opsforecast.Metric {
	switch s {
	case "req_rate":
		return opsforecast.MetricReqRate
	case "latency_p95":
		return opsforecast.MetricLatency
	default:
		return opsforecast.MetricErrRate
	}
}

// recommend derives a bounded, deterministic action hint. No scaling
// executor in v1 — the recommendation is the deliverable (owner can wire
// a system_command catalog entry + HITL approval later).
func recommend(f *opsforecast.Forecast, m opsforecast.Metric) string {
	if f == nil {
		return ""
	}
	switch {
	case f.ETABuckets == 0 && f.HitIn24h && f.Threshold > 0:
		return fmt.Sprintf("threshold %.0f already breached — page now, check capacity + recent deploys", f.Threshold)
	case !f.HitIn24h || f.ETABuckets < 0:
		return ""
	case f.Confidence < 40:
		return "trend noisy (low confidence) — widen the window or watch, don't act yet"
	case m == opsforecast.MetricErrRate && f.Level == opsforecast.LevelRising:
		return fmt.Sprintf("error rate rising — pre-check error budgets and rollback candidate; ~%.0f buckets to threshold", f.ETABuckets)
	case m == opsforecast.MetricLatency && f.Level == opsforecast.LevelRising:
		return fmt.Sprintf("latency trending to %.0fms — warm capacity (scale-out candidate), check slowest routes; ~%.0f buckets", f.Threshold, f.ETABuckets)
	case m == opsforecast.MetricReqRate && f.Level == opsforecast.LevelRising:
		return fmt.Sprintf("traffic ramping — plan scale-out before %.0f reqs/bucket; ~%.0f buckets", f.Threshold, f.ETABuckets)
	}
	return ""
}

// NewOpsForecast builds the predictive ops tool. Same resolvePath
// containment boundary as analyze_log (Hard Rule 2).
func NewOpsForecast(workspace string) (Tool, error) {
	return New(Config{
		Name:        "ops_forecast",
		Description: "Predictive ops forecast from a log's timestamps (deterministic, no LLM): buckets the last lines into time windows, fits a trend, and reports when a metric (err_rate | req_rate | latency_p95) crosses a threshold — with a recommended action.",
	}, func(ctx context.Context, in OpsForecastToolInput) (OpsForecastToolOutput, error) {
		out := OpsForecastToolOutput{}
		if in.Path == "" {
			return out, fmt.Errorf("path is required")
		}
		metric := in.Metric
		if metric == "" {
			metric = "err_rate"
		}
		if in.Threshold <= 0 {
			// sensible default per metric so casual calls still get an ETA
			switch metricOf(metric) {
			case opsforecast.MetricLatency:
				in.Threshold = 1000 // ms
			default:
				in.Threshold = 10 // per-bucket count
			}
		}
		w := optPos(&in.WindowMin, 1)
		if w > 60 {
			w = 60
		}
		n := optPos(&in.MaxLines, 2000)
		if n > 20000 {
			n = 20000
		}
		real, err := resolvePath(workspace, in.Path)
		if err != nil {
			return out, err
		}
		window := time.Duration(w) * time.Minute
		buckets, skipped, err := opsforecast.BuildBuckets(real, n, window)
		if err != nil {
			return out, err
		}
		fc, err := opsforecast.ForecastMetric(metricOf(metric), buckets, window, in.Threshold)
		if err != nil {
			return out, err
		}
		var b strings.Builder
		b.WriteString(fc.String())
		if skipped > 0 {
			fmt.Fprintf(&b, " (%d lines without timestamps skipped)", skipped)
		}
		return OpsForecastToolOutput{
			Metric:            metric,
			Path:              in.Path,
			Buckets:           fc.Buckets,
			WindowMin:         w,
			Skipped:           skipped,
			Summary:           b.String(),
			RecommendedAction: recommend(fc, metricOf(metric)),
		}, nil
	})
}
