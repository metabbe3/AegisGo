package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aegisgo/internal/store"
)

// TestDashboardHealthCard pins the self-audit card: wired Health with a
// non-empty snapshot renders error share + per-source confidence; the
// card is absent when Health is nil, errors, or has zero runs (liveness
// surface first — no card rather than a misleading "0 runs · 0% err").
func TestDashboardHealthCard(t *testing.T) {
	snap := &store.HealthSnapshot{
		TotalRuns: 10,
		Errors:    2,
		BySource: map[string]store.SourceHealth{
			"regex_router": {Runs: 8, Errors: 0, AvgConf: 100},
			"llm":          {Runs: 2, Errors: 2, AvgConf: 55},
		},
	}
	h := dashboardHandler(DashboardDeps{Health: fakeHealth{snap: snap}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	for _, want := range []string{"self-audit", "10 runs", "20% err", "regex_router conf", "llm conf"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

// TestDashboardHealthCardOmittedWhenNil covers the degrade contract.
func TestDashboardHealthCardOmittedWhenNil(t *testing.T) {
	h := dashboardHandler(DashboardDeps{}) // no Health wired
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "self-audit") {
		t.Fatal("self-audit card rendered with nil Health")
	}
}

// TestDashboardHealthCardOmittedWhenEmpty pins the zero-runs omit: a
// fresh install shows no card instead of a meaningless 0%/0 line.
func TestDashboardHealthCardOmittedWhenEmpty(t *testing.T) {
	h := dashboardHandler(DashboardDeps{Health: fakeHealth{snap: &store.HealthSnapshot{}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "self-audit") {
		t.Fatal("self-audit card rendered with zero runs")
	}
}
