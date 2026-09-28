package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aegisgo/internal/tools"
)

// fakeJobStore backs the cancel-endpoint tests: it remembers the id it was
// asked to stop and replays the trio (snapshot, known, issued).
type fakeJobStore struct {
	capture string
	known   bool
	issued  bool
}

func (f fakeJobStore) ListJobs() []Job { return nil }

func (f *fakeJobStore) CancelJob(id string) (Job, bool, bool) {
	f.capture = id
	return Job{ID: id, Kind: "download", Status: "running"}, f.known, f.issued
}

// TestJobsCancelEndpointIssued: POST /v1/jobs/{id}/cancel on a running job
// answers 200 with the pre-cancel snapshot (status still running — the flip
// is async) and cancel_issued=true. The wire shape must stay honest.
func TestJobsCancelEndpointIssued(t *testing.T) {
	st := &fakeJobStore{known: true, issued: true}
	h := Handler(Deps{Jobs: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/jobs/j_1/cancel", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if st.capture != "j_1" {
		t.Fatalf("store saw %q, want j_1", st.capture)
	}
	var env struct {
		Data cancelJobResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	body := env.Data
	if !body.CancelIssued || body.Status != tools.JobRunning {
		t.Fatalf("body = %+v, want issued=true with running snapshot", body)
	}
}

// TestJobsCancelEndpointUnknown: an unknown id is a 404 with the id echoed,
// not a 200 pretending something happened.
func TestJobsCancelEndpointUnknown(t *testing.T) {
	st := &fakeJobStore{known: false}
	h := Handler(Deps{Jobs: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/jobs/j_404/cancel", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "j_404") {
		t.Fatalf("body should echo the id: %s", rec.Body.String())
	}
}

// TestJobsCancelEndpointUnwired: nil store keeps the route mounted but
// answers 503 — same degrade contract as GET /v1/jobs.
func TestJobsCancelEndpointUnwired(t *testing.T) {
	h := Handler(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/jobs/j_1/cancel", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestJobsCancelEndpointAlreadyFinished: known-but-finished is still 200
// with cancel_issued=false — the caller learns nothing was sent.
func TestJobsCancelEndpointAlreadyFinished(t *testing.T) {
	st := &fakeJobStore{known: true, issued: false}
	h := Handler(Deps{Jobs: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/jobs/j_2/cancel", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Data cancelJobResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	body := env.Data
	if body.CancelIssued {
		t.Fatal("cancel_issued must be false for an already-finished job")
	}
}
