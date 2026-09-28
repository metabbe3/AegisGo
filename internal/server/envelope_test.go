package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Envelope contract (owner directive 28 Sep): every /v1 response is
// {success, code, message?, reason?, data?} — errors carry a stable code
// token AND a human message.

func TestErrEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusUnauthorized, "missing or invalid bearer token")
	var e Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Success || e.Code != "UNAUTHORIZED" || e.Reason != "auth" {
		t.Fatalf("envelope = %+v", e)
	}
	if e.Message == "" {
		t.Fatal("error envelope must carry a human message")
	}
}

func TestOKEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOK(rec, http.StatusOK, map[string]int{"n": 1})
	var e Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !e.Success || e.Code != "OK" || e.Reason != "" {
		t.Fatalf("envelope = %+v", e)
	}
	if e.Data == nil {
		t.Fatal("success envelope must carry data")
	}
}

func TestCodeTableStable(t *testing.T) {
	cases := map[int]string{
		http.StatusOK:                  "OK",
		http.StatusBadRequest:          "BAD_REQUEST",
		http.StatusUnauthorized:        "UNAUTHORIZED",
		http.StatusNotFound:            "NOT_FOUND",
		http.StatusInternalServerError: "INTERNAL",
	}
	for status, code := range cases {
		if codeFor(status) != code {
			t.Errorf("codeFor(%d) = %q, want %q", status, codeFor(status), code)
		}
	}
}

func TestProviderFailureReasonOnRunError(t *testing.T) {
	// The sync-run handler classifies engine errors as provider failures,
	// with the engine's failure text as the message.
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusInternalServerError,
		errEnvelope(http.StatusInternalServerError, "LLM run failed: 529", ReasonProvider))
	var e Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Success || e.Code != "INTERNAL" || e.Reason != "provider" {
		t.Fatalf("envelope = %+v", e)
	}
}
