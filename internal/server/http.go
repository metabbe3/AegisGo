// Package server exposes the hybrid engine over a small JSON REST API so
// other microservices can call it. Deliberately stdlib-only: one binary, no
// framework, tiny footprint on a Linux box.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// WebhookPath is where Deps.Webhook mounts. Named for Telegram (the only
// webhook transport today) but the handler is opaque to this package.
const WebhookPath = "/telegram/webhook"

// Engine is the slice of *engine.Engine the HTTP layer needs.
type Engine interface {
	Run(ctx context.Context, prompt string) engine.Result
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
	Engine   Engine
	Answers  AnswerStore
	Readines Readiness // optional; nil skips the deep check
	// Webhook, when non-nil, is mounted at POST /telegram/webhook (the
	// handler itself is built by internal/telegram; the server stays
	// transport-agnostic).
	Webhook http.Handler
	Logger  *slog.Logger
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
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Readines != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := d.Readines.Ping(ctx); err != nil {
				writeError(w, http.StatusServiceUnavailable, "store unreachable: "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("POST /v1/agent/run", func(w http.ResponseWriter, r *http.Request) {
		runAgent(w, r, d)
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

func runAgent(w http.ResponseWriter, r *http.Request, d Deps) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
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
		go func(ctx context.Context) {
			res := d.Engine.Run(ctx, req.Prompt)
			status := store.AnswerDone
			if res.DecisionSource == store.SourceError {
				status = store.AnswerError
			}
			if err := d.Answers.CompleteAnswer(context.Background(), traceID, status, res.Answer); err != nil {
				d.Logger.Error("completing answer", "trace_id", traceID, "error", err)
			}
		}(context.WithoutCancel(r.Context())) // outlive the request, keep trace values
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
