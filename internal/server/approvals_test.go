package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aegisgo/internal/server"
	"aegisgo/internal/store"
)

// realStore opens :memory: — the handlers are thin; the store CAS is the
// behavior under test, so a fake would test the fake.
func newApprovalsHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := server.Handler(server.Deps{Approvals: st})
	return h, st
}

func TestListApprovalsEmpty(t *testing.T) {
	h, _ := newApprovalsHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/approvals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var body struct{ Count int `json:"count"` }
	json.NewDecoder(rec.Body).Decode(&body)
	if body.Count != 0 {
		t.Fatalf("count = %d", body.Count)
	}
}

func TestDecideApproveFlow(t *testing.T) {
	h, st := newApprovalsHandler(t)
	id, err := st.CreateApproval(context.Background(), "system_command", "{}", "rest test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"decision": "approve", "by": "curl"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/approvals/1/decision", bytes.NewReader(body))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	a, ok, _ := st.GetApproval(context.Background(), id)
	if !ok || a.State != "approved" || a.DecidedBy != "curl" {
		t.Fatalf("ledger = %+v", a)
	}
}

func TestDecideConflictOnDouble(t *testing.T) {
	h, st := newApprovalsHandler(t)
	st.CreateApproval(context.Background(), "k", "{}", "r")
	body, _ := json.Marshal(map[string]string{"decision": "deny"})
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/approvals/1/decision", bytes.NewReader(body)))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/approvals/1/decision", bytes.NewReader(body))) // CAS loss
	if rec1.Code != http.StatusOK || rec2.Code != http.StatusConflict {
		t.Fatalf("codes = %d then %d", rec1.Code, rec2.Code)
	}
}

func TestDecideBadBody(t *testing.T) {
	h, _ := newApprovalsHandler(t)
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/v1/approvals/abc/decision", `{"decision":"approve"}`, 400},
		{"/v1/approvals/1/decision", `{"decision":"maybe"}`, 400},
		{"/v1/approvals/999/decision", `{"decision":"approve"}`, 404},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", tc.path, bytes.NewReader([]byte(tc.body)))
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s → %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestApprovalsUnwired503(t *testing.T) {
	h := server.Handler(server.Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/approvals", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
}
