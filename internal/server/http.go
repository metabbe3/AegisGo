// Package server exposes the hybrid engine over a small JSON REST API so
// other microservices can call it. Deliberately stdlib-only: one binary, no
// framework, tiny footprint on a Linux box.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	StatsWindow(ctx context.Context, days int) (*store.StatsSnapshot, error)
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

// JobStore is the background-jobs surface the REST API needs: list
// snapshots, cancel a running one. *tools.JobManager satisfies it via
// adapters; keeping it named (not inline) lets both surfaces grow
// together without churning every Deps literal in tests.
type JobStore interface {
	ListJobs() []Job
	CancelJob(id string) (Job, bool, bool)
}

// cancelJobResponse is the POST /v1/jobs/{id}/cancel body: the snapshot
// taken at cancel time (still "running" — the flip happens in the job's
// own goroutine) plus whether a cancellation was actually issued.
type cancelJobResponse struct {
	Job
	CancelIssued bool `json:"cancel_issued"`
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
	Tasks *task.Group
	// Approvals backs the HITL REST endpoints (ADR-0008); nil = the
	// endpoints answer 503 rather than 404-ing silently.
	Approvals ApprovalSource
	Logger    *slog.Logger
	// Dashboard, when non-nil, mounts the mini status page at GET /
	// (nil keeps / unmounted — embedders choose).
	Dashboard *DashboardDeps
	// Jobs exposes background-job snapshots (download jobs) at
	// GET /v1/jobs and cancels a running one at POST /v1/jobs/{id}/cancel;
	// nil = 503 (serve wires it when tools run).
	Jobs JobStore
	// Events, when non-nil, mounts GET /v1/events (SSE): one RunEvent per
	// completed engine run, pushed live. nil = 503 like other unwired routes.
	Events *EventPub
	// AuthToken, when non-empty, gates every /v1/* route behind
	// "Authorization: Bearer <token>" (constant-time). Empty = open
	// (the LAN default). Health probes and the dashboard stay public.
	AuthToken string
}

// bearerAuth wraps h, enforcing a constant-time Bearer match. An empty
// token disables the gate entirely (fail-open is the documented default;
// the knob exists for exposure beyond localhost).
func bearerAuth(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			h.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(got, prefix) ||
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized,
				map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		h.ServeHTTP(w, r)
	})
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

	if d.Dashboard != nil {
		mux.Handle("GET /{$}", dashboardHandler(*d.Dashboard))
	}

	// /v1/* sits behind the optional bearer gate; probes and the
	// dashboard stay public (uptime checks must not need credentials).
	v1 := http.NewServeMux()
	mux.Handle("/v1/", bearerAuth(d.AuthToken, v1))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Readiness != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := d.Readiness.Ping(ctx); err != nil {
				writeErr(w, http.StatusServiceUnavailable, "store unreachable: "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	v1.HandleFunc("POST /v1/agent/run", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			runAgentStream(w, r, d)
			return
		}
		runAgent(w, r, d)
	})

	v1.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		eventsHandler(w, r, d)
	})

	v1.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		statsHandler(w, r, d)
	})

	v1.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if d.Jobs == nil {
			writeErr(w, http.StatusServiceUnavailable, "jobs not wired")
			return
		}
		jobs := d.Jobs.ListJobs()
		if jobs == nil {
			jobs = []Job{} // JSON: [] not null
		}
		writeOK(w, http.StatusOK, jobs)
	})

	// POST /v1/jobs/{id}/cancel — abort a running background job. The
	// status flip lands asynchronously in the job's own goroutine, so the
	// response carries the pre-cancel snapshot plus "cancel issued": the
	// honest wire shape for a fire-then-converge operation.
	v1.HandleFunc("POST /v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if d.Jobs == nil {
			writeErr(w, http.StatusServiceUnavailable, "jobs not wired")
			return
		}
		id := r.PathValue("id")
		j, known, issued := d.Jobs.CancelJob(id)
		if !known {
			writeErr(w, http.StatusNotFound, "unknown job "+id)
			return
		}
		writeOK(w, http.StatusOK, cancelJobResponse{Job: j, CancelIssued: issued})
	})

	v1.HandleFunc("GET /v1/answers/{trace}", func(w http.ResponseWriter, r *http.Request) {
		getAnswer(w, r, d)
	})

	v1.HandleFunc("GET /v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		if d.Approvals == nil {
			writeErr(w, http.StatusServiceUnavailable, "approvals not wired")
			return
		}
		listApprovals(w, r, d.Approvals)
	})

	v1.HandleFunc("POST /v1/approvals/{id}/decision", func(w http.ResponseWriter, r *http.Request) {
		if d.Approvals == nil {
			writeErr(w, http.StatusServiceUnavailable, "approvals not wired")
			return
		}
		decideApproval(w, r, d.Approvals)
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
	Confidence     int    `json:"confidence"`
	ConfBand       string `json:"confidence_band"`
}

// decodeRunRequest parses and validates the run body shared by the sync and
// stream handlers: a 1 MiB cap and the same two 400s. false means the
// response is already written.
func decodeRunRequest(w http.ResponseWriter, r *http.Request) (runRequest, bool) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return req, false
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
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
			writeErr(w, http.StatusInternalServerError, "queueing answer: "+err.Error())
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
		writeOK(w, http.StatusAccepted, map[string]string{"trace_id": traceID, "status": store.AnswerPending})
		return
	}

	// One HTTP request maps to one bounded run so a stuck provider call
	// can't pin a worker forever.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res := d.Engine.Run(ctx, req.Prompt)
	code := http.StatusOK
	if res.DecisionSource == store.SourceError {
		// The engine's answer text IS the failure message ("LLM run
		// failed: ..."); envelope it rather than burying it in data.
		writeJSON(w, http.StatusInternalServerError,
			errEnvelope(http.StatusInternalServerError, res.Answer, ReasonProvider))
		return
	}
	writeOK(w, code, runResponse{
		Output:         res.Answer,
		DecisionSource: res.DecisionSource,
		TraceID:        res.TraceID,
		LatencyMS:      res.LatencyMS,
		Confidence:     res.Confidence.Score,
		ConfBand:       res.Confidence.Band,
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
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
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
		"confidence":      res.Confidence.Score,
		"confidence_band": res.Confidence.Band,
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

// statsHandler is GET /v1/stats — the observability surface. Optional
// ?days=N narrows the audit/corpus aggregates to the last N days
// (1-3650); the default stays all-time so existing consumers are stable.
func statsHandler(w http.ResponseWriter, r *http.Request, d Deps) {
	if d.Stats == nil {
		writeErr(w, http.StatusNotImplemented, "stats unavailable")
		return
	}
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			writeErr(w, http.StatusBadRequest, "days must be an integer 1-3650")
			return
		}
		days = n
	}
	s, err := d.Stats.StatsWindow(r.Context(), days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, http.StatusOK, s)
}

// getAnswer polls an async run: pending | done | error | 404.
func getAnswer(w http.ResponseWriter, r *http.Request, d Deps) {
	traceID := r.PathValue("trace")
	if traceID == "" {
		writeErr(w, http.StatusBadRequest, "trace is required")
		return
	}
	a, ok, err := d.Answers.GetAnswer(r.Context(), traceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no answer for trace "+traceID)
		return
	}
	writeOK(w, http.StatusOK, a)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		// Nothing more we can do; the status code is already sent.
		_ = err
	}
}
