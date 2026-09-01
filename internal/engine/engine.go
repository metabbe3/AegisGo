// Package engine is the hybrid decision pipeline: trace → deterministic
// router → native execution → LLM fallback. Every interface (CLI today,
// REST, gRPC, Telegram later) calls Engine.Run; every run is audited with
// its decision_source.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

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

// PromoteAfter is the consecutive-agreement streak that promotes a shadow
// rule to active. A field so tests (and only tests) can lower it.
var PromoteAfter = 5

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
// trace ID (trace.New); Router hits never touch the LLM — except shadow
// evaluations, which deliberately run both (see Decision.Evaluate).
func (e *Engine) Run(ctx context.Context, prompt string) Result {
	traceID := trace.From(ctx)
	start := time.Now()

	// 1. Deterministic router — free, instant.
	d := e.Router.Handle(ctx, prompt)
	if d.Handled && !d.Evaluate {
		return e.finishRouter(ctx, d, prompt, traceID, start)
	}

	// 2. Kill switch: provider outages become a fast "no", not an incident.
	//    (Shadow evaluation also degrades here — nothing to compare against.)
	if e.LLM == nil {
		if d.Handled {
			// Rule matched but shadow/sampled needs an LLM: answer from the
			// rule anyway (never worse than the fallback).
			return e.finishRouter(ctx, d, prompt, traceID, start)
		}
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

	// 3a. Shadow evaluation: the LLM runs; the rule is compared, not
	//     necessarily answered from (Decision.AnswerFromRule decides).
	if d.Handled && d.Evaluate {
		resp, err := e.LLM.RunText(ctx, prompt).Collect()
		latency := ms(time.Since(start))
		if err != nil {
			// Comparison impossible; the rule's answer is the safe path.
			return e.finishRouter(ctx, d, prompt, traceID, start)
		}
		llmTools := ToolNames(resp)
		agreed := containsTool(llmTools, d.Tool)
		if err := e.Store.RecordShadow(ctx, store.ShadowEvent{
			RuleName: d.RuleID, TraceID: traceID, Agreed: agreed, LLMTools: llmTools,
		}); err != nil {
			slog.WarnContext(ctx, "recording shadow event", "rule", d.RuleID, "error", err)
		}
		e.evaluateLifecycle(ctx, d, agreed)

		if d.AnswerFromRule {
			// Sampled active rule: the rule answers; the LLM only checked.
			return e.finishRouter(ctx, d, prompt, traceID, start)
		}
		// Shadow rule: the LLM answers while the rule learns.
		res := Result{Answer: resp.String(), DecisionSource: store.SourceLLM,
			RuleID: d.RuleID, TraceID: traceID, LatencyMS: latency}
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceLLM, RuleID: d.RuleID,
			Prompt: prompt, Model: e.Model, LatencyMS: latency,
		})
		e.recordFallback(ctx, traceID, prompt, resp)
		return res
	}

	// 3b. LLM fallback — the only path that costs money on a miss.
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
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLM,
		Prompt: prompt, Model: e.Model, LatencyMS: latency,
	})
	e.recordFallback(ctx, traceID, prompt, resp)
	return Result{Answer: resp.String(), DecisionSource: store.SourceLLM, TraceID: traceID, LatencyMS: latency}
}

// streamChunkLen is the slice size for deterministic answers streamed
// through the same SSE contract as LLM tokens — one client shape for every
// path.
const streamChunkLen = 256

// RunStreaming is Run with an emit callback: every path delivers its
// output incrementally. Router hits chunk their deterministic answer;
// shadow evaluations fall back to a single synchronous chunk (comparison
// logic stays in Run); LLM fallbacks forward text deltas as they arrive.
func (e *Engine) RunStreaming(ctx context.Context, prompt string, emit func(chunk string) error) Result {
	traceID := trace.From(ctx)
	start := time.Now()

	d := e.Router.Handle(ctx, prompt)

	// Deterministic path (including sampled evaluations whose answer comes
	// from the rule): slice the known answer.
	if d.Handled && !d.Evaluate {
		res := e.finishRouter(ctx, d, prompt, traceID, start)
		emitChunks(res.Answer, emit)
		return res
	}
	if d.Handled && d.Evaluate && d.AnswerFromRule {
		// Sampled active rule — rule answers; run the comparison inline.
		e.compareInline(ctx, d, prompt)
		res := e.finishRouter(ctx, d, prompt, traceID, start)
		emitChunks(res.Answer, emit)
		return res
	}
	if d.Handled && d.Evaluate {
		// Shadow-state rule: run synchronously (LLM answers), emit once.
		res := e.Run(ctx, prompt)
		emitChunks(res.Answer, emit)
		return res
	}
	if e.LLM == nil {
		res := Result{
			Answer:         "No deterministic rule matched and the LLM fallback is disabled (AEGIS_LLM=off).",
			DecisionSource: store.SourceLLMOff, TraceID: traceID,
		}
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceLLMOff,
			Prompt: prompt, LatencyMS: ms(time.Since(start)),
		})
		emitChunks(res.Answer, emit)
		return res
	}

	// LLM fallback: forward deltas as they stream off the provider.
	stream := e.LLM.RunText(ctx, prompt)
	var acc strings.Builder
	tools := map[string]bool{}
	var usage message.UsageDetails
	var streamErr error
	for upd, err := range stream {
		if err != nil {
			streamErr = err
			break
		}
		if upd == nil {
			continue
		}
		if text := upd.Contents.Text(); text != "" {
			acc.WriteString(text)
			if err := emit(text); err != nil {
				streamErr = err
				break
			}
		}
		for _, c := range upd.Contents {
			if fc, ok := c.(*message.FunctionCallContent); ok && fc.Name != "" {
				tools[fc.Name] = true
			}
		}
		usage.Add(upd.Contents.Usage())
	}
	latency := ms(time.Since(start))
	if streamErr != nil {
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceError,
			Prompt: prompt, Model: e.Model, LatencyMS: latency, Outcome: "error",
		})
		res := Result{Answer: fmt.Sprintf("LLM run failed: %s", streamErr),
			DecisionSource: store.SourceError, TraceID: traceID, LatencyMS: latency}
		emitChunks(res.Answer, emit)
		return res
	}

	var toolNames []string
	for n := range tools {
		toolNames = append(toolNames, n)
	}
	e.Store.RecordFallback(store.FallbackEvent{
		TraceID:          traceID,
		NormalizedPrompt: store.NormalizePrompt(prompt),
		RawPrompt:        prompt,
		ToolsUsed:        strings.Join(toolNames, ","),
		Model:            e.Model,
		TokensIn:         int(usage.InputTokenCount),
		TokensOut:        int(usage.OutputTokenCount),
	})
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLM,
		Prompt: prompt, Model: e.Model, LatencyMS: latency,
	})
	return Result{Answer: acc.String(), DecisionSource: store.SourceLLM,
		TraceID: traceID, LatencyMS: latency}
}

// compareInline runs the sampled-active-rule shadow comparison without
// changing the answer path (the rule already answered).
func (e *Engine) compareInline(ctx context.Context, d router.Decision, prompt string) {
	if e.LLM == nil {
		return
	}
	resp, err := e.LLM.RunText(ctx, prompt).Collect()
	if err != nil {
		return // comparison unavailable; the rule's answer stands
	}
	llmTools := ToolNames(resp)
	agreed := containsTool(llmTools, d.Tool)
	if err := e.Store.RecordShadow(ctx, store.ShadowEvent{
		RuleName: d.RuleID, TraceID: trace.From(ctx), Agreed: agreed, LLMTools: llmTools,
	}); err != nil {
		slog.WarnContext(ctx, "recording shadow event", "rule", d.RuleID, "error", err)
	}
	e.evaluateLifecycle(ctx, d, agreed)
}

func emitChunks(s string, emit func(string) error) {
	if s == "" {
		return
	}
	for len(s) > streamChunkLen {
		if err := emit(s[:streamChunkLen]); err != nil {
			return
		}
		s = s[streamChunkLen:]
	}
	_ = emit(s)
}

// finishRouter audits and returns a rule-sourced answer.
func (e *Engine) finishRouter(ctx context.Context, d router.Decision, prompt, traceID string, start time.Time) Result {
	res := Result{RuleID: d.RuleID, TraceID: traceID, LatencyMS: ms(time.Since(start))}
	if d.Err != nil {
		res.Answer = fmt.Sprintf("command %q failed: %s", d.RuleID, d.Err)
		// The rule matched; a tool failing underneath it is still a router
		// decision (Hard Rule 6), so Result reports the source like the audit row.
		res.DecisionSource = store.SourceRouter
		e.audit(ctx, store.AuditEvent{
			TraceID: traceID, DecisionSource: store.SourceRouter, RuleID: d.RuleID,
			Prompt: prompt, LatencyMS: res.LatencyMS, Outcome: "error",
		})
		return res
	}
	res.Answer = d.Text()
	res.DecisionSource = store.SourceRouter
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceRouter, RuleID: d.RuleID,
		Prompt: prompt, LatencyMS: res.LatencyMS,
	})
	return res
}

// recordFallback persists the mining corpus: normalized shape plus the
// tools the LLM actually chose (the miner's dominant-tool signal).
func (e *Engine) recordFallback(ctx context.Context, traceID, prompt string, resp *agent.Response) {
	tin, tout := tokenUsage(resp)
	e.Store.RecordFallback(store.FallbackEvent{
		TraceID:          traceID,
		NormalizedPrompt: store.NormalizePrompt(prompt),
		RawPrompt:        prompt,
		ToolsUsed:        strings.Join(ToolNames(resp), ","),
		Model:            e.Model,
		TokensIn:         tin,
		TokensOut:        tout,
	})
}

// evaluateLifecycle applies promotion/demotion after a shadow comparison.
// Divergence ALWAYS demotes — the guard that keeps the cost dashboard from
// rewarding wrong rules.
func (e *Engine) evaluateLifecycle(ctx context.Context, d router.Decision, agreed bool) {
	if !agreed {
		if err := e.Store.SetRuleState(ctx, d.RuleID, store.RuleStateDemoted, false); err != nil {
			slog.WarnContext(ctx, "demoting rule failed", "rule", d.RuleID, "error", err)
		} else {
			slog.WarnContext(ctx, "rule demoted: LLM chose different tools",
				"rule", d.RuleID, "expected_tool", d.Tool)
		}
		return
	}
	streak, err := e.Store.ShadowStreak(ctx, d.RuleID)
	if err != nil {
		slog.WarnContext(ctx, "reading shadow streak", "rule", d.RuleID, "error", err)
		return
	}
	if streak >= PromoteAfter {
		if err := e.Store.SetRuleState(ctx, d.RuleID, store.RuleStateActive, true); err != nil {
			slog.WarnContext(ctx, "promoting rule failed", "rule", d.RuleID, "error", err)
		} else {
			slog.InfoContext(ctx, "mined rule promoted to active",
				"rule", d.RuleID, "streak", streak)
		}
	}
}

// ToolNames extracts the tool names the LLM called in a response — the
// deterministic signal shadow comparison uses (no judge model needed).
func ToolNames(resp *agent.Response) []string {
	if resp == nil {
		return nil
	}
	var names []string
	for _, m := range resp.Messages {
		for _, c := range m.Contents {
			if fc, ok := c.(*message.FunctionCallContent); ok && fc.Name != "" {
				names = append(names, fc.Name)
			}
		}
	}
	return names
}

// tokenUsage sums usage content across the response's messages.
func tokenUsage(resp *agent.Response) (int, int) {
	if resp == nil {
		return 0, 0
	}
	var u message.UsageDetails
	for _, m := range resp.Messages {
		u.Add(m.Contents.Usage())
	}
	return int(u.InputTokenCount), int(u.OutputTokenCount)
}

func containsTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
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
