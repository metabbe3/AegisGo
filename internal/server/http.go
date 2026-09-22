// Package server exposes the hybrid engine over a small JSON REST API so
// other microservices can call it. Deliberately stdlib-only: one binary, no
// framework, tiny footprint on a Linux box.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/logx"
	"aegisgo/internal/store"
	"aegisgo/internal/task"
	"aegisgo/internal/trace"
)

// WebhookPath is where Deps.Webhook mounts. Named for Telegram (the only
// webhook transport today) but the handler is opaque to this package.
const WebhookPath = "/telegram/webhook"

// Engine is the slice of *engine.Engine the HTTP layer needs.
type Engine interface {
	Run(ctx context.Context, prompt string) engine.Result
	RunStreaming(ctx context.Context, prompt string, emit func(chunk string) error) engine.Result
}

// StatsSource computes the stats snapshot.
type StatsSource interface {
	Stats(ctx context.Context) (*store.StatsSnapshot, error)
}

// AnswerStore is the async answer store the API polls.
type AnswerStore interface {
	PutAnswer(ctx context.Context, traceID string) error
	CompleteAnswer(ctx context.Context, traceID, status, output string) error
	GetAnswer(ctx context.Context, traceID string) (store.Answer, bool, error)
}

// Readiness reports whether the service can serve traffic.
type Readiness interface {
	Ping(ctx context.Context) error
}

// Deps bundles the handler dependencies.
type Deps struct {
	Engine    Engine
	Answers   AnswerStore
	Readiness Readiness // optional; nil skips the deep check
	Stats     StatsSource
	// Webhook, when non-nil, is mounted at POST /telegram/webhook (the
	// handler itself is built by internal/telegram; the server stays
	// transport-agnostic).
	Webhook http.Handler
	// Tasks tracks async-run goroutines so shutdown can join them (BC5).
	// nil is fine for tests and embedders that never wait: Handler fills in
	// a throwaway group.
	Tasks  *task.Group
	Logger *slog.Logger
}

// Handler builds the HTTP routes.
//
// Routes:
//
//	GET  /healthz              liveness — process is up
//	GET  /readyz               readiness — store reachable, engine wired
//	POST /v1/agent/run         run the hybrid pipeline (sync by default)
//	POST /v1/agent/run?async=1 enqueue; returns 202 + trace_id
//	GET  /v1/answers/{trace}   poll an async answer
func Handler(d Deps) http.Handler {
	d.Logger = logx.Or(d.Logger)
	if d.Tasks == nil {
		d.Tasks = &task.Group{}
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Readiness != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := d.Readiness.Ping(ctx); err != nil {
				writeError(w, http.StatusServiceUnavailable, "store unreachable: "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("POST /v1/agent/run", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			runAgentStream(w, r, d)
			return
		}
		runAgent(w, r, d)
	})

	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		statsHandler(w, r, d)
	})

	mux.HandleFunc("GET /v1/answers/{trace}", func(w http.ResponseWriter, r *http.Request) {
		getAnswer(w, r, d)
	})

	if d.Webhook != nil {
		mux.Handle("POST "+WebhookPath, d.Webhook)
	}

	return traceMiddleware(d.Logger, mux)
}

// traceMiddleware mints (or accepts) a trace ID per request and carries it
// in context; the engine's audit rows join on it.
func traceMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID, ctx := trace.New(r.Context(), r.Header.Get("X-Trace-Id"))
		w.Header().Set("X-Trace-Id", traceID)
		logger.InfoContext(ctx, "request accepted",
			"method", r.Method, "path", r.URL.Path, "trace_id", traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// runRequest is the body of POST /v1/agent/run.
type runRequest struct {
	// Prompt is the user message. Required.
	Prompt string `json:"prompt"`
}

// runResponse is the sync result of POST /v1/agent/run.
type runResponse struct {
	Output         string `json:"output"`
	DecisionSource string `json:"decision_source"`
	TraceID        string `json:"trace_id"`
	LatencyMS      int64  `json:"latency_ms"`
}

// decodeRunRequest parses and validates the run body shared by the sync and
// stream handlers: a 1 MiB cap and the same two 400s. false means the
// response is already written.
func decodeRunRequest(w http.ResponseWriter, r *http.Request) (runRequest, bool) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return req, false
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return req, false
	}
	return req, true
}

func runAgent(w http.ResponseWriter, r *http.Request, d Deps) {
	req, ok := decodeRunRequest(w, r)
	if !ok {
		return
	}

	// Router hits return in milliseconds; LLM fallbacks can take a minute.
	// Async mode acks immediately and parks the answer under its trace ID.
	if r.URL.Query().Get("async") == "1" {
		traceID := trace.From(r.Context())
		if err := d.Answers.PutAnswer(r.Context(), traceID); err != nil {
			writeError(w, http.StatusInternalServerError, "queueing answer: "+err.Error())
			return
		}
		// Same 5-minute bound as the sync path: a stuck provider call must
		// not park a pending answer forever. Completion persists on a fresh
		// context so it survives even a timed-out run.
		d.Tasks.Go(r.Context(), 5*time.Minute, func(ctx context.Context) {
			res := d.Engine.Run(ctx, req.Prompt)
			status := store.AnswerStatusFor(res.DecisionSource)
			if err := d.Answers.CompleteAnswer(context.Background(), traceID, status, res.Answer); err != nil {
				d.Logger.Error("completing answer", "trace_id", traceID, "error", err)
			}
		}) // detached: outlives the request, keeps trace values
		writeJSON(w, http.StatusAccepted, map[string]string{"trace_id": traceID, "status": store.AnswerPending})
		return
	}

	// One HTTP request maps to one bounded run so a stuck provider call
	// can't pin a worker forever.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res := d.Engine.Run(ctx, req.Prompt)
	code := http.StatusOK
	if res.DecisionSource == store.SourceError {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, runResponse{
		Output:         res.Answer,
		DecisionSource: res.DecisionSource,
		TraceID:        res.TraceID,
		LatencyMS:      res.LatencyMS,
	})
}

// runAgentStream is POST /v1/agent/run?stream=1: server-sent events with
// `data:` chunks, terminated by a final event carrying the run metadata.
// Both router hits (deterministic, chunked) and LLM runs (provider deltas)
// use the same wire shape.
func runAgentStream(w http.ResponseWriter, r *http.Request, d Deps) {
	req, ok := decodeRunRequest(w, r)
	if !ok {
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

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var chunks int
	res := d.Engine.RunStreaming(ctx, req.Prompt, func(chunk string) error {
		chunks++
		return sseWrite(w, fl, "delta", map[string]string{"text": chunk})
	})
	_ = sseWrite(w, fl, "done", map[string]any{
		"decision_source": res.DecisionSource,
		"rule_id":         res.RuleID,
		"trace_id":        res.TraceID,
		"latency_ms":      res.LatencyMS,
		"chunks":          chunks,
	})
}

// sseWrite emits one SSE event as JSON.
func sseWrite(w http.ResponseWriter, fl http.Flusher, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	fl.Flush()
	return nil
}

// statsHandler is GET /v1/stats — the observability surface.
func statsHandler(w http.ResponseWriter, r *http.Request, d Deps) {
	if d.Stats == nil {
		writeError(w, http.StatusNotImplemented, "stats unavailable")
		return
	}
	s, err := d.Stats.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// getAnswer polls an async run: pending | done | error | 404.
func getAnswer(w http.ResponseWriter, r *http.Request, d Deps) {
	traceID := r.PathValue("trace")
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "trace is required")
		return
	}
	a, ok, err := d.Answers.GetAnswer(r.Context(), traceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no answer for trace "+traceID)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		// Nothing more we can do; the status code is already sent.
		_ = err
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
