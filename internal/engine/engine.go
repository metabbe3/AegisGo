// Package engine is the hybrid decision pipeline: trace → deterministic
// router → native execution → LLM fallback. Every interface (CLI today,
// REST, gRPC, Telegram later) calls Engine.Run; every run is audited with
// its decision_source.
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/microsoft/agent-framework-go/agent"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// LLMRunner is the slice of *agent.Agent the engine needs; an interface so
// tests (and AEGIS_LLM=off) can run without a provider.
type LLMRunner interface {
	RunText(ctx context.Context, msg string, options ...agent.Option) agent.ResponseStream
}

// Result is one completed run.
type Result struct {
	Answer         string
	DecisionSource string
	RuleID         string
	TraceID        string
	LatencyMS      int64
}

// Engine wires the router, the optional LLM fallback, and the audit store.
type Engine struct {
	Router *router.Router
	LLM    LLMRunner // nil => AEGIS_LLM=off: misses return fast
	Store  *store.Store
	// IFace labels audit rows ("cli", "rest", …) — the engine's default.
	// Interfaces sharing one engine override per call via WithIFace.
	IFace string
	// Model names the LLM on audit rows ("" when disabled).
	Model string
}

type ifaceKey struct{}

// WithIFace labels the run's audit rows with the calling interface (e.g.
// store.IFaceTelegram) regardless of which engine instance serves it.
func WithIFace(ctx context.Context, iface string) context.Context {
	return context.WithValue(ctx, ifaceKey{}, iface)
}

// Run executes the hybrid pipeline for one prompt. The ctx must carry a
// trace ID (trace.New); Router hits never touch the LLM.
func (e *Engine) Run(ctx context.Context, prompt string) Result {
	traceID := trace.From(ctx)
	start := time.Now()

	// 1. Deterministic router — free, instant.
	if d := e.Router.Handle(ctx, prompt); d.Handled {
		res := Result{RuleID: d.RuleID, TraceID: traceID}
		if d.Err != nil {
			res.Answer = fmt.Sprintf("command %q failed: %s", d.RuleID, d.Err)
			e.audit(ctx, store.AuditEvent{
				TraceID: traceID, DecisionSource: store.SourceRouter, RuleID: d.RuleID,
				Prompt: prompt, LatencyMS: ms(time.Since(start)), Outcome: "error",
			})
			return res
		}
		res.Answer = d.Text()
		res.DecisionSource = store.SourceRouter
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceRouter, RuleID: d.RuleID,
			Prompt: prompt, LatencyMS: ms(time.Since(start)),
		})
		return res
	}

	// 2. Kill switch: provider outages become a fast "no", not an incident.
	if e.LLM == nil {
		res := Result{
			Answer:         "No deterministic rule matched and the LLM fallback is disabled (AEGIS_LLM=off).",
			DecisionSource: store.SourceLLMOff,
			TraceID:        traceID,
		}
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceLLMOff,
			Prompt: prompt, LatencyMS: ms(time.Since(start)),
		})
		return res
	}

	// 3. LLM fallback — the only path that costs money.
	resp, err := e.LLM.RunText(ctx, prompt).Collect()
	latency := ms(time.Since(start))
	if err != nil {
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceError,
			Prompt: prompt, Model: e.Model, LatencyMS: latency, Outcome: "error",
		})
		return Result{
			Answer:         fmt.Sprintf("LLM run failed: %s", err),
			DecisionSource: store.SourceError,
			TraceID:        traceID,
			LatencyMS:      latency,
		}
	}
	answer := resp.String()

	// The fallback corpus feeds the Phase-3 rule miner.
	e.Store.RecordFallback(store.FallbackEvent{
		TraceID:          traceID,
		NormalizedPrompt: store.NormalizePrompt(prompt),
		RawPrompt:        prompt,
		Model:            e.Model,
	})
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLM,
		Prompt: prompt, Model: e.Model, LatencyMS: latency,
	})
	return Result{Answer: answer, DecisionSource: store.SourceLLM, TraceID: traceID, LatencyMS: latency}
}

func (e *Engine) audit(ctx context.Context, ev store.AuditEvent) {
	if e.Store == nil {
		return
	}
	if ev.Interface == "" {
		if v, ok := ctx.Value(ifaceKey{}).(string); ok && v != "" {
			ev.Interface = v
		} else {
			ev.Interface = e.IFace
		}
	}
	e.Store.Audit(ctx, ev)
}

func ms(d time.Duration) int64 { return d.Milliseconds() }
