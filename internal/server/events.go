package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// RunEvent is one completed engine run, as seen by /v1/events subscribers.
// It mirrors the audit line (decision_source is the §6 contract) minus the
// prompt — events are a health surface, not a log replay.
type RunEvent struct {
	DecisionSource string `json:"decision_source"`
	TraceID        string `json:"trace_id"`
	RuleID         string `json:"rule_id,omitempty"`
	LatencyMS      int64  `json:"latency_ms"`
}

// EventPub is the publisher side the run middleware holds; the handler side
// lives in the same registry value.
type EventPub struct {
	mu   sync.Mutex
	subs map[chan RunEvent]struct{}
}

// NewEventPub returns an empty registry.
func NewEventPub() *EventPub {
	return &EventPub{subs: make(map[chan RunEvent]struct{})}
}

// Publish fans an event out to every subscriber. Slow consumers are dropped,
// never block the run path: a stale dashboard is acceptable, a stalled
// engine is not (Hard Rule 5's spirit, applied to reads).
func (p *EventPub) Publish(e RunEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- e:
		default: // buffer full — this subscriber loses the event
		}
	}
}

// Subscribe registers a listener until ctx is done. The channel is buffered
// so Publish stays non-blocking; a consumer that falls more than 16 events
// behind silently misses the overflow.
func (p *EventPub) Subscribe(ctx context.Context) <-chan RunEvent {
	ch := make(chan RunEvent, 16)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
		// Drain so a late Publish's non-blocking send never wedges.
		for range ch {
		}
	}()
	return ch
}

// eventsHandler is GET /v1/events: an SSE stream of RunEvents. The stream
// ends only when the client disconnects (r.Context) — the handler writes a
// comment heartbeat to survive proxies that close idle connections.
func eventsHandler(w http.ResponseWriter, r *http.Request, d Deps) {
	if d.Events == nil {
		writeError(w, http.StatusServiceUnavailable, "events not wired")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ch := d.Events.Subscribe(r.Context())
	heart := time.NewTicker(15 * time.Second)
	defer heart.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			// A comment frame keeps middleboxes from reaping the stream.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case e := <-ch:
			if err := sseWrite(w, fl, "run", e); err != nil {
				return
			}
		}
	}
}
