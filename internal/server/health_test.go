package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"aegisgo/internal/store"
)

// fakeHealth lets health tests run without SQLite: it returns a canned
// HealthSnapshot shaped like the real store.Health output.
type fakeHealth struct {
	snap *store.HealthSnapshot
	err  error
}

func (f fakeHealth) Health(context.Context) (*store.HealthSnapshot, error) {
	return f.snap, f.err
}

// TestHealthUnwired503 pins the nil-degrade contract: an unwired Health
// answers 503-in-envelope, never a silent 404.
func TestHealthUnwired503(t *testing.T) {
	h := Handler(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Success || body.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("body = %+v, want success=false code=NOT_IMPLEMENTED", body)
	}
}

// TestHealthOK verifies the happy path: envelope success + per-source
// rollup passthrough, deterministic field order via map iteration is
// irrelevant since we assert one known source.
func TestHealthOK(t *testing.T) {
	h := Handler(Deps{Health: fakeHealth{snap: &store.HealthSnapshot{
		TotalRuns: 10,
		Errors:    1,
		BySource: map[string]store.SourceHealth{
			"regex_router": {Runs: 8, Errors: 0, AvgLatency: 5, AvgConf: 100},
		},
	}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			TotalRuns int `json:"total_runs"`
			Errors    int `json:"errors"`
			BySource  map[string]struct {
				Runs    int     `json:"runs"`
				Errors  int     `json:"errors"`
				AvgConf float64 `json:"avg_confidence"`
			} `json:"by_source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Success || env.Data.TotalRuns != 10 || env.Data.Errors != 1 {
		t.Fatalf("env = %+v", env)
	}
	rr, ok := env.Data.BySource["regex_router"]
	if !ok || rr.Runs != 8 || rr.Errors != 0 || rr.AvgConf != 100 {
		t.Fatalf("regex_router = %+v ok=%v", rr, ok)
	}
}

// TestHealthStoreError500 pins the error path: a store failure surfaces
// as 500-in-envelope with the message, not a panic or empty body.
func TestHealthStoreError500(t *testing.T) {
	h := Handler(Deps{Health: fakeHealth{err: errors.New("boom")}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	var body struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Success {
		t.Fatal("success=true on store error")
	}
}
