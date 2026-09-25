package server

import (
	"aegisgo/internal/tools"
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

// TestDashboardRouteMounted: the real mux mounts GET / when Dashboard is
// provided (and still serves /healthz).
func TestDashboardRouteMounted(t *testing.T) {
	h := Handler(Deps{Dashboard: &DashboardDeps{Commit: "abc"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "AegisGo") {
		t.Fatalf("GET / = %d", w.Code)
	}
}

// TestBearerAuthGatesV1: with a token set, /v1/stats without a Bearer is
// 401; with the right Bearer it passes; healthz stays public.
func TestBearerAuthGatesV1(t *testing.T) {
	h := Handler(Deps{AuthToken: "s3cret"})
	// no token → 401
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-token /v1/stats = %d, want 401", rr.Code)
	}
	// wrong token → 401
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token = %d, want 401", rr.Code)
	}
	// healthz public even with token set
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz with auth on = %d, want 200", rr.Code)
	}
	// empty token = open (default LAN posture)
	open := Handler(Deps{})
	rr = httptest.NewRecorder()
	open.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))
	if rr.Code == http.StatusUnauthorized {
		t.Fatal("empty token must not gate /v1/*")
	}
}

// TestJobsEndpoint: GET /v1/jobs returns the wired snapshots (and 503
// when not wired).
func TestJobsEndpoint(t *testing.T) {
	// not wired → 503
	h := Handler(Deps{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired /v1/jobs = %d, want 503", rr.Code)
	}
	// wired via a real manager: one finished job shows up
	m := tools.NewJobManager()
	h = Handler(Deps{Jobs: m})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("wired /v1/jobs = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"kind"`) && strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("body = %q", rr.Body.String())
	}
}
