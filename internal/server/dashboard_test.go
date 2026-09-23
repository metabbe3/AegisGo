package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// fakeDashStats pins a deterministic snapshot for dashboard tests.
type fakeDashStats struct{}

func (fakeDashStats) Stats(ctx context.Context) (*store.StatsSnapshot, error) {
	return &store.StatsSnapshot{
		TotalRuns:      42,
		DeflectionRate: 0.5,
		BySource:       map[string]int{"regex_router": 21, "llm_disabled": 21},
		RulesByState:   map[string]int{"seed": 6, "mined": 3},
	}, nil
}

// TestDashboardRendersCoreLines: uptime, stats cards, build footer.
func TestDashboardRendersCoreLines(t *testing.T) {
	h := dashboardHandler(DashboardDeps{
		Stats:     fakeDashStats{},
		StartedAt: time.Now().Add(-90 * time.Second),
		Commit:    "deadbee",
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	body := w.Body.String()
	for _, want := range []string{"AegisGo", "1m30s", "deadbee", "regex_router", "21", "mined rules", "auto-refresh 30s"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	if hdr := w.Header().Get("Cache-Control"); hdr != "no-store" {
		t.Fatalf("cache-control = %q", hdr)
	}
}

// TestDashboardNilStatsStillRenders: stats failure must not blank the page —
// the dashboard is a liveness surface first.
func TestDashboardNilStatsStillRenders(t *testing.T) {
	h := dashboardHandler(DashboardDeps{Commit: "x"})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(w.Body.String(), "AegisGo") {
		t.Fatal("nil stats blanked the dashboard")
	}
}
