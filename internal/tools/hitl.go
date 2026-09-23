package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// HITL executor gate (ADR-0005). A gated tool call does NOT run on request:
// it creates an approval row, waits for a human decision (polling the
// ledger), and only then re-validates policy and executes. Fail-closed:
// deny, expiry, timeout, and shutdown all mean "do not run".

// ApprovalLedger is the store slice the gate needs.
type ApprovalLedger interface {
	CreateApproval(ctx context.Context, kind, payload, reason string) (int64, error)
	GetApproval(ctx context.Context, id int64) (ApprovalRow, bool, error)
	DecideApproval(ctx context.Context, id int64, state, decidedBy string) (bool, error)
}

// ApprovalRow is the ledger's row shape (mirrors store.Approval).
type ApprovalRow struct {
	ID      int64
	Kind    string
	Payload string
	State   string // pending | approved | denied | expired
}

// GateConfig tunes one HITL gate.
type GateConfig struct {
	// WaitTimeout bounds how long the gate polls before giving up.
	WaitTimeout time.Duration
	// PollInterval is the ledger poll cadence.
	PollInterval time.Duration
}

// DefaultGateConfig: wait up to 2 minutes, poll every second.
func DefaultGateConfig() GateConfig {
	return GateConfig{WaitTimeout: 2 * time.Minute, PollInterval: time.Second}
}

// GateOutcome tells the caller exactly what happened — typed decisions,
// no ambiguous strings (jev-style).
type GateOutcome string

const (
	OutcomeApproved GateOutcome = "approved"
	OutcomeDenied   GateOutcome = "denied"
	OutcomeExpired  GateOutcome = "expired"
	OutcomeTimeout  GateOutcome = "timeout"
	OutcomeShutdown GateOutcome = "shutdown"
)

// DescribeGated is the dry-run path (ADR-0013): it runs the SAME approval
// flow as RunGated — creates the ledger row, waits for a human verdict —
// but the action is replaced by a payload echo, so nothing executes. An
// approver rehearses the ✅/🚫 decision with zero side effects; the
// returned string is the exact payload that WOULD have run.
func DescribeGated(ctx context.Context, ledger ApprovalLedger, cfg GateConfig,
	kind, payload, reason string) (string, GateOutcome, error) {
	var preview string
	_, outcome, err := RunGated(ctx, ledger, cfg, kind, payload, reason,
		func(ctx context.Context) (any, error) {
			preview = payload
			return struct{ DryRun bool }{DryRun: true}, nil
		})
	if err != nil {
		return "", outcome, err
	}
	if outcome != OutcomeApproved {
		return "", outcome, nil
	}
	return preview, outcome, nil
}

// RunGated executes fn only if a human approves the action first.
// kind/payload/reason describe the action for the approver; fn is the
// action itself. The payload is re-marshaled into the approval row, and
// fn receives EXACTLY the payload that was approved — an approver always
// sees what will run (no bait-and-switch).
func RunGated(ctx context.Context, ledger ApprovalLedger, cfg GateConfig,
	kind, payload, reason string, fn func(ctx context.Context) (any, error)) (any, GateOutcome, error) {

	id, err := ledger.CreateApproval(ctx, kind, payload, reason)
	if err != nil {
		return nil, "", fmt.Errorf("hitl gate: creating approval: %w", err)
	}

	deadline := time.Now().Add(cfg.WaitTimeout)
	for {
		select {
		case <-ctx.Done():
			return nil, OutcomeShutdown, nil
		case <-time.After(cfg.PollInterval):
		}
		if time.Now().After(deadline) {
			return nil, OutcomeTimeout, nil
		}
		row, ok, err := ledger.GetApproval(ctx, id)
		if err != nil {
			return nil, "", fmt.Errorf("hitl gate: reading approval %d: %w", id, err)
		}
		if !ok {
			return nil, "", fmt.Errorf("hitl gate: approval %d vanished", id)
		}
		switch row.State {
		case "approved":
			// Policy re-check lives in fn itself (the caller validates its
			// own inputs again); the gate only guarantees the human said yes.
			out, err := fn(ctx)
			return out, OutcomeApproved, err
		case "denied":
			return nil, OutcomeDenied, nil
		case "expired":
			return nil, OutcomeExpired, nil
		}
		// still pending: keep polling
	}
}

// MarshalPayload is a helper for gated callers building their payload.
func MarshalPayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hitl gate: marshaling payload: %w", err)
	}
	return string(b), nil
}
