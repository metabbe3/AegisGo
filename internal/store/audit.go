package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"
)

// Decision sources — the contract every request path reports. These are the
// spec's audit fields: every row and log line carries one.
const (
	SourceRouter = "regex_router" // deterministic rule matched, native Go executed
	SourceLLM    = "llm"          // no rule matched, LLM fallback answered
	SourceLLMOff = "llm_disabled" // no rule matched and AEGIS_LLM=off
	SourceError  = "error"
)

// Interface names recorded on audit rows.
const (
	IFaceCLI  = "cli"
	IFaceREST = "rest"
)

// AuditEvent is one row of the decision trail.
type AuditEvent struct {
	TraceID        string
	Interface      string
	DecisionSource string
	RuleID         string // empty for LLM rows
	Prompt         string // hashed on write; never stored raw
	Model          string
	TokensIn       int
	TokensOut      int
	LatencyMS      int64
	Outcome        string // "ok" | "error"
}

// Audit is the dual-sink entry point: one slog line (ops console/systemd
// journal) and one durable row (queryable corpus), same fields. The prompt
// is hashed — raw prompts never land in the audit table; the fallback corpus
// stores its own normalized copy under its own policy.
func (s *Store) Audit(ctx context.Context, ev AuditEvent) {
	if ev.Outcome == "" {
		ev.Outcome = "ok"
	}
	now := time.Now().UTC()
	slog.InfoContext(ctx, "request",
		"decision_source", ev.DecisionSource,
		"trace_id", ev.TraceID,
		"interface", ev.Interface,
		"rule_id", ev.RuleID,
		"model", ev.Model,
		"latency_ms", ev.LatencyMS,
		"outcome", ev.Outcome,
	)
	s.exec(`INSERT INTO audit_events
		(ts, trace_id, interface, decision_source, rule_id, prompt_sha256, model, tokens_in, tokens_out, latency_ms, outcome)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		now.Format(time.RFC3339Nano),
		ev.TraceID,
		ev.Interface,
		ev.DecisionSource,
		nullable(ev.RuleID),
		hashPrompt(ev.Prompt),
		nullable(ev.Model),
		ev.TokensIn,
		ev.TokensOut,
		ev.LatencyMS,
		ev.Outcome,
	)
}

// RecordFallback persists an LLM-fallback event as mining corpus (Phase 3
// clusters these into candidate router rules). The normalized prompt
// placeholder-izes volatile details so repeated shapes cluster together.
func (s *Store) RecordFallback(ev FallbackEvent) {
	s.exec(`INSERT INTO fallback_events
		(ts, trace_id, normalized_prompt, raw_prompt, tools_used, answer_sha256, model, tokens_in, tokens_out)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		ev.TraceID,
		ev.NormalizedPrompt,
		ev.RawPrompt,
		nullable(ev.ToolsUsed),
		nullable(ev.AnswerSHA256),
		nullable(ev.Model),
		ev.TokensIn,
		ev.TokensOut,
	)
}

// FallbackEvent is one LLM fallback observation.
type FallbackEvent struct {
	TraceID          string
	NormalizedPrompt string
	RawPrompt        string
	ToolsUsed        string // comma-joined tool names
	AnswerSHA256     string
	Model            string
	TokensIn         int
	TokensOut        int
}

// NormalizePrompt replaces volatile fragments (numbers, quoted strings,
// likely paths) with placeholders so semantically identical prompts cluster.
// This is the Phase-3 miner's clustering key; quality here decides rule
// quality there.
func NormalizePrompt(p string) string {
	return normalize(p)
}

func hashPrompt(p string) string {
	if p == "" {
		return ""
	}
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:8]) // short hash: dedupe/correlation, not security
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
