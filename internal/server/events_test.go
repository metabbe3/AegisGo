package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Registry semantics: publish reaches the subscriber, slow consumers drop
// instead of blocking, unsubscribe on ctx-done leaves no leak.
func TestEventPubDeliverDropUnsubscribe(t *testing.T) {
	p := NewEventPub()
	ctx, cancel := context.WithCancel(context.Background())
	ch := p.Subscribe(ctx)

	p.Publish(RunEvent{DecisionSource: "regex_router", TraceID: "t1"})
	select {
	case e := <-ch:
		if e.DecisionSource != "regex_router" || e.TraceID != "t1" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}

	// Slow consumer: fill the 16-slot buffer, then one more publish must
	// not block and the overflow is lost (not fatal, not stuck).
	for i := 0; i < 20; i++ {
		p.Publish(RunEvent{TraceID: "fill"})
	}
	done := make(chan struct{})
	go func() { p.Publish(RunEvent{TraceID: "overflow"}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// Unsubscribe: after cancel, the registry drains without wedging.
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.subs)
		p.mu.Unlock()
		if n == 0 {
			return // clean
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("subscriber not removed after ctx cancel")
}

// eventsHandler streams at least one run event plus the 503 when unwired.
func TestEventsHandlerStreamAnd503(t *testing.T) {
	h := Handler(Deps{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/events", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired /v1/events = %d, want 503", rr.Code)
	}

	p := NewEventPub()
	h2 := Handler(Deps{Events: p})
	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	*req = *req.WithContext(ctx)
	go func() {
		// give the handler a beat to subscribe, then push and close
		time.Sleep(50 * time.Millisecond)
		p.Publish(RunEvent{DecisionSource: "llm", TraceID: "t9", LatencyMS: 42})
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	h2.ServeHTTP(rr2, req)
	body := rr2.Body.String()
	if !strings.Contains(body, "event: run") || !strings.Contains(body, `"trace_id":"t9"`) {
		t.Fatalf("body = %q", body)
	}
	if ct := rr2.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}

// TestEventsHandlerNonFlusher: a response writer without Flush support
// gets a clean 500, not a panic.
type noFlushRW struct{ *httptest.ResponseRecorder }

func TestEventsHandlerNonFlusher(t *testing.T) {
	p := NewEventPub()
	h := Handler(Deps{Events: p})
	rr := httptest.NewRecorder()
	w := struct{ http.ResponseWriter }{rr} // hides Flusher
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/events", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("non-flusher = %d, want 500", rr.Code)
	}
}
