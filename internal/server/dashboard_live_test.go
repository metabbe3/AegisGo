package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The dashboard page must embed the live-feed client: it subscribes to
// /v1/events and prepends run lines under the stats cards. The page stays
// functional without JS (rows simply never appear) — this test pins the
// wiring, not the JS execution.
func TestDashboardEmbedsLiveFeed(t *testing.T) {
	h := dashboardHandler(DashboardDeps{
		StartedAt: time.Now().Add(-90 * time.Second),
		Commit:    "deadbeef",
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/", nil))

	page := w.Body.String()
	for _, marker := range []string{
		`id="live"`,                     // container element
		`new EventSource('/v1/events')`, // SSE subscription
		`'run'`,                         // event name the client listens for
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("dashboard page missing %q", marker)
		}
	}
	// meta-refresh stays: numeric stats refresh via reload; the feed only
	// carries run events (deliberate split, see handoff batch-5 item 1).
	if !strings.Contains(page, `http-equiv="refresh"`) {
		t.Fatal("live feed must not replace the 30s meta-refresh")
	}
}

// Rows render newest-first and the list is bounded — a long-lived browser
// tab must not accumulate unbounded DOM (Hard Rule 10, applied to the page).
func TestDashboardFeedRowTemplate(t *testing.T) {
	h := dashboardHandler(DashboardDeps{Commit: "x"})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	page := w.Body.String()
	for _, marker := range []string{
		`f.insertBefore(row,f.firstElementChild)`, // prepend = newest first
		`f.children.length >= 8`,                  // bounded list
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("dashboard feed script missing %q", marker)
		}
	}
}
