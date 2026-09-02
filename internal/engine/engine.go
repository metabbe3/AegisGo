// Package engine is the hybrid decision pipeline: trace → deterministic
// router → native execution → LLM fallback. Every interface (CLI today,
// REST, gRPC, Telegram later) calls Engine.Run; every run is audited with
// its decision_source.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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

// Classifier is the fast tier consulted on router misses BEFORE the smart
// LLM (Dify's question-classify pattern): one cheap call that either
// resolves the prompt by picking a native tool (ok=true, answer returned)
// or declines. Implementations must never fail loudly — declining simply
// falls through to the LLM fallback, exactly like Dify's deterministic
// first-class fallback.
type Classifier interface {
	Classify(ctx context.Context, prompt string) (answer, tool string, ok bool)
}

// Result is one completed run.
type Result struct {
	Answer         string
	DecisionSource string
	RuleID         string
	TraceID        string
	LatencyMS      int64
}

// Header renders the one-line decision header every human interface shows
// before the answer: "[source via rule<sep>123ms]", or "[source<sep>123ms]"
// when no rule fired. sep joins the fields — ", " for terminals, " · " for
// Telegram — so the cost behavior stays visible on both.
func (r Result) Header(sep string) string {
	if r.RuleID != "" {
		return fmt.Sprintf("[%s via %s%s%dms]", r.DecisionSource, r.RuleID, sep, r.LatencyMS)
	}
	return fmt.Sprintf("[%s%s%dms]", r.DecisionSource, sep, r.LatencyMS)
}

// PromoteAfter is the consecutive-agreement streak that promotes a shadow
// rule to active. A field so tests (and only tests) can lower it.
var PromoteAfter = 5

// Engine wires the router, the optional fast-tier classifier, the LLM
// fallback, and the audit store.
type Engine struct {
	Router *router.Router
	LLM    LLMRunner // nil => AEGIS_LLM=off: misses return fast
	// Classifier is the optional fast tier on router misses (nil = off).
	// It only ever runs when the LLM is on — the kill switch is absolute.
	Classifier Classifier
	// ClassifierModel names the fast-tier model on audit rows ("" when off).
	ClassifierModel string
	Store           *store.Store
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
		return e.finishLLMOff(ctx, traceID, prompt, start)
	}

	// 2.5 Fast tier: on a miss, the classifier may resolve the prompt with
	//     one cheap native-tool call (AEGIS_CLASSIFIER=on). Only reachable
	//     when the LLM is on; declining falls through to the fallback below.
	if !d.Handled && e.Classifier != nil {
		if answer, tool, ok := e.Classifier.Classify(ctx, prompt); ok {
			return e.finishClassifier(ctx, answer, tool, traceID, prompt, start)
		}
	}

	// 3a. Shadow evaluation: the LLM runs; the rule is compared, not
	//     necessarily answered from (Decision.AnswerFromRule decides).
	if d.Handled && d.Evaluate {
		return e.finishShadow(ctx, d, prompt, traceID, start)
	}

	// 3b. LLM fallback — the only path that costs money on a miss.
	resp, err := e.LLM.RunText(ctx, prompt).Collect()
	latency := ms(time.Since(start))
	if err != nil {
		return e.finishLLMError(ctx, err, traceID, prompt, start)
	}
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLM,
		Prompt: prompt, Model: e.Model, LatencyMS: latency,
	})
	e.recordFallback(traceID, prompt, resp)
	return Result{Answer: resp.String(), DecisionSource: store.SourceLLM, TraceID: traceID, LatencyMS: latency}
}

// streamChunkLen is the slice size for deterministic answers streamed
// through the same SSE contract as LLM tokens — one client shape for every
// path.
const streamChunkLen = 256

// llmOffAnswer is the kill-switch miss message (AEGIS_LLM=off).
const llmOffAnswer = "No deterministic rule matched and the LLM fallback is disabled (AEGIS_LLM=off)."

// RunStreaming is Run with an emit callback: every path delivers its
// output incrementally. Router hits chunk their deterministic answer;
// shadow evaluations run synchronously (the LLM's answer, one chunk);
// LLM fallbacks forward text deltas as they arrive. Every branch shares
// Run's tail helpers, so the decision/audit contract is spelled once.
func (e *Engine) RunStreaming(ctx context.Context, prompt string, emit func(chunk string) error) Result {
	traceID := trace.From(ctx)
	start := time.Now()

	d := e.Router.Handle(ctx, prompt)

	// Deterministic path: slice the router's answer. This covers sampled
	// evaluations (compareInline runs the check alongside) and shadow rules
	// without an LLM (nothing to compare — the rule answers, Run's degrade).
	if d.Handled && (e.LLM == nil || !d.Evaluate || d.AnswerFromRule) {
		if d.Evaluate {
			e.compareInline(ctx, d, prompt)
		}
		res := e.finishRouter(ctx, d, prompt, traceID, start)
		emitChunks(res.Answer, emit)
		return res
	}

	// Shadow evaluation with an LLM: finishShadow consumes the ALREADY-
	// ROUTED decision, so the tool executes exactly once (never re-route).
	if d.Handled {
		res := e.finishShadow(ctx, d, prompt, traceID, start)
		emitChunks(res.Answer, emit)
		return res
	}

	if e.LLM == nil {
		res := e.finishLLMOff(ctx, traceID, prompt, start)
		emitChunks(res.Answer, emit)
		return res
	}

	// Fast tier (same placement as Run: miss + LLM on). The deterministic
	// answer is chunked through the same SSE contract as everything else.
	if e.Classifier != nil {
		if answer, tool, ok := e.Classifier.Classify(ctx, prompt); ok {
			res := e.finishClassifier(ctx, answer, tool, traceID, prompt, start)
			emitChunks(res.Answer, emit)
			return res
		}
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
		// Deltas were already streamed; the failure notice is chunked too.
		res := e.finishLLMError(ctx, streamErr, traceID, prompt, start)
		emitChunks(res.Answer, emit)
		return res
	}

	// Sorted so corpus rows are deterministic despite the map's random
	// iteration order (same tool set either way).
	toolNames := slices.Sorted(maps.Keys(tools))
	e.recordFallbackParts(traceID, prompt, strings.Join(toolNames, ","), e.Model,
		int(usage.InputTokenCount), int(usage.OutputTokenCount))
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
	e.compareShadow(ctx, d, prompt)
}

// compareShadow runs the LLM for an evaluate-rule, records the tool-choice
// agreement, and advances the rule's lifecycle. It never chooses the
// answer; nil means the comparison was unavailable (the rule's answer
// stands).
func (e *Engine) compareShadow(ctx context.Context, d router.Decision, prompt string) *agent.Response {
	resp, err := e.LLM.RunText(ctx, prompt).Collect()
	if err != nil {
		return nil // comparison unavailable; the rule's answer stands
	}
	llmTools := ToolNames(resp)
	agreed := slices.Contains(llmTools, d.Tool)
	if err := e.Store.RecordShadow(ctx, store.ShadowEvent{
		RuleName: d.RuleID, TraceID: trace.From(ctx), Agreed: agreed, LLMTools: llmTools,
	}); err != nil {
		slog.WarnContext(ctx, "recording shadow event", "rule", d.RuleID, "error", err)
	}
	e.evaluateLifecycle(ctx, d, agreed)
	return resp
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

// recordFallback persists the mining corpus from a collected LLM response:
// normalized shape plus the tools the LLM actually chose (the miner's
// dominant-tool signal).
func (e *Engine) recordFallback(traceID, prompt string, resp *agent.Response) {
	tin, tout := tokenUsage(resp)
	e.recordFallbackParts(traceID, prompt, strings.Join(ToolNames(resp), ","), e.Model, tin, tout)
}

// recordFallbackParts persists one mining-corpus row from raw parts — the
// spelling every fallback site (sync LLM, streaming LLM, classifier) shares.
func (e *Engine) recordFallbackParts(traceID, prompt, toolsUsed, model string, tokensIn, tokensOut int) {
	e.Store.RecordFallback(store.FallbackEvent{
		TraceID:          traceID,
		NormalizedPrompt: store.NormalizePrompt(prompt),
		RawPrompt:        prompt,
		ToolsUsed:        toolsUsed,
		Model:            model,
		TokensIn:         tokensIn,
		TokensOut:        tokensOut,
	})
}

// finishLLMOff is the kill-switch miss: no rule matched and there is no
// provider to ask. The run still audits (decision_source=llm_disabled).
func (e *Engine) finishLLMOff(ctx context.Context, traceID, prompt string, start time.Time) Result {
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLMOff,
		Prompt: prompt, LatencyMS: ms(time.Since(start)),
	})
	return Result{Answer: llmOffAnswer, DecisionSource: store.SourceLLMOff, TraceID: traceID}
}

// finishClassifier records a fast-tier hit. Classifier hits are
// tool-choice-labeled corpus: the miner graduates recurring shapes into
// regex rules, so known patterns decay from one cheap call to zero LLM
// calls. No token counts — the fast tier's usage isn't collected here.
func (e *Engine) finishClassifier(ctx context.Context, answer, tool, traceID, prompt string, start time.Time) Result {
	latency := ms(time.Since(start))
	e.recordFallbackParts(traceID, prompt, tool, e.ClassifierModel, 0, 0)
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLMClassifier,
		Prompt: prompt, Model: e.ClassifierModel, LatencyMS: latency,
	})
	return Result{Answer: answer, DecisionSource: store.SourceLLMClassifier,
		TraceID: traceID, LatencyMS: latency}
}

// finishLLMError is the fallback's failure tail: a Result that still
// reports decision_source=error (Hard Rule 6) plus its audit row.
func (e *Engine) finishLLMError(ctx context.Context, err error, traceID, prompt string, start time.Time) Result {
	latency := ms(time.Since(start))
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

// finishShadow completes an evaluate-rule (shadow or sampled) from the
// ALREADY-ROUTED decision d — the router has executed d's tool once, so
// this must never re-route (double tool side effects). The LLM runs for
// comparison; the rule answers when it is sampled (AnswerFromRule) or the
// comparison is unavailable, else the LLM's answer wins while the rule
// learns. Callers must guarantee e.LLM != nil.
func (e *Engine) finishShadow(ctx context.Context, d router.Decision, prompt, traceID string, start time.Time) Result {
	resp := e.compareShadow(ctx, d, prompt)
	if resp == nil || d.AnswerFromRule {
		// Comparison unavailable, or sampled active rule: the rule answers;
		// the LLM only checked.
		return e.finishRouter(ctx, d, prompt, traceID, start)
	}
	latency := ms(time.Since(start))
	res := Result{Answer: resp.String(), DecisionSource: store.SourceLLM,
		RuleID: d.RuleID, TraceID: traceID, LatencyMS: latency}
	e.audit(ctx, store.AuditEvent{
		TraceID: traceID, DecisionSource: store.SourceLLM, RuleID: d.RuleID,
		Prompt: prompt, Model: e.Model, LatencyMS: latency,
	})
	e.recordFallback(traceID, prompt, resp)
	return res
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
