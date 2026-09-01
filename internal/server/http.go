// Package server exposes the agent over a small JSON REST API so other
// microservices can call it. Deliberately stdlib-only: one binary, no
// framework, tiny footprint on a Linux box.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
)

// Runner is the slice of *agent.Agent the HTTP layer needs. Keeping it an
// interface makes the handlers testable without a live provider.
type Runner interface {
	RunText(ctx context.Context, msg string, options ...agent.Option) agent.ResponseStream
}

// Handler builds the HTTP routes for a runner. A nil logger falls back to
// slog.Default so callers (and tests) can omit it.
func Handler(runner Runner, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/agent/run", func(w http.ResponseWriter, r *http.Request) {
		runAgent(w, r, runner, logger)
	})
	return mux
}

// runRequest is the body of POST /v1/agent/run.
type runRequest struct {
	// Prompt is the user message for the agent. Required.
	Prompt string `json:"prompt"`
}

// runResponse is the result of POST /v1/agent/run.
type runResponse struct {
	// Output is the agent's final reply text.
	Output string `json:"output"`
}

func runAgent(w http.ResponseWriter, r *http.Request, runner Runner, logger *slog.Logger) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	// One HTTP request maps to one agent run with a bounded lifetime, so a
	// stuck provider call can't pin a worker forever.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	logger.Info("agent run started")
	resp, err := runner.RunText(ctx, req.Prompt).Collect()
	if err != nil {
		logger.Error("agent run failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Info("agent run finished")
	writeJSON(w, http.StatusOK, runResponse{Output: resp.String()})
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
