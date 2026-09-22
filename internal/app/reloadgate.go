package app

import (
	"context"
	"fmt"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/telegram"
	"aegisgo/internal/tools"
)

// Gated reload-rules (ADR-0007): the first REAL L2 consumer of RunGated.
// Reloading the router's rule set changes agent behavior at runtime —
// exactly the class of action that must pass a human before it runs
// (L-tier §L, ADR-0002). It is also perfectly safe to deny: the previous
// rules stay live.

// ReloadGate wires the L2 path without import cycles: the store is both
// ledger (approvals) and rule source; the router is the swap target.
// ReloadGate is the first REAL L2 gated action (ADR-0007).
type ReloadGate struct {
	Store  *store.Store
	Router *router.Router
	// QuickTimeout overrides the wait for tests (0 = default 10 min).
	QuickTimeout time.Duration
}

// GateConfigFor returns the gate tuning for reloads: humans answer on
// their phone; 10 minutes is generous without letting a request rot.
func (g *ReloadGate) GateConfig() tools.GateConfig {
	wait := 10 * time.Minute
	if g.QuickTimeout > 0 {
		wait = g.QuickTimeout
	}
	return tools.GateConfig{WaitTimeout: wait, PollInterval: 10 * time.Millisecond}
}

// Request creates the approval row (called when policy says L2).
func (g *ReloadGate) Request(ctx context.Context, reason string) (int64, error) {
	payload, err := tools.MarshalPayload(map[string]string{"action": "reload_rules"})
	if err != nil {
		return 0, err
	}
	return g.Store.CreateApproval(ctx, "reload_rules", payload, reason)
}

// Run executes the reload once a human approved. It re-reads the rules
// table and swaps the live router; a bad row set keeps the previous
// rules (router.Swap validates), so the worst case is "nothing changed",
// reported honestly.
func (g *ReloadGate) Run(ctx context.Context) (any, error) {
	defs, err := router.LoadRules(ctx, g.Store)
	if err != nil {
		return nil, fmt.Errorf("reload_rules: loading: %w", err)
	}
	if err := g.Router.Swap(defs); err != nil {
		return nil, fmt.Errorf("reload_rules: swap rejected: %w", err)
	}
	return map[string]any{"reloaded": true, "rules": len(defs)}, nil
}

// Handle is the full gated flow for one request: ledger → wait → execute
// on approve. Outcome typed per RunGated (approved/denied/timeout/…).
func (g *ReloadGate) Handle(ctx context.Context, reason string) (any, tools.GateOutcome, error) {
	id, err := g.Request(ctx, reason)
	if err != nil {
		return nil, "", err
	}
	// Poll the ledger for THIS approval via RunGated: create is already
	// done, so pass a ledger view that returns the existing row.
	lv := &existingApproval{st: g.Store, id: id}
	out, outcome, err := tools.RunGated(ctx, lv, g.GateConfig(), "reload_rules", "{}", reason,
		g.Run)
	return out, outcome, err
}

// existingApproval adapts the store so RunGated (which creates its own
// row) instead waits on an existing one — the notifier already told the
// human about id.
type existingApproval struct {
	st *store.Store
	id int64
}

func (e *existingApproval) CreateApproval(ctx context.Context, kind, payload, reason string) (int64, error) {
	return e.id, nil // already created; report the same id
}

func (e *existingApproval) GetApproval(ctx context.Context, id int64) (tools.ApprovalRow, bool, error) {
	a, ok, err := e.st.GetApproval(ctx, id)
	if err != nil || !ok {
		return tools.ApprovalRow{}, false, err
	}
	return tools.ApprovalRow{ID: a.ID, Kind: a.Kind, Payload: a.Payload, State: a.State}, true, nil
}

func (e *existingApproval) DecideApproval(ctx context.Context, id int64, state, by string) (bool, error) {
	return e.st.DecideApproval(ctx, id, state, by)
}

var _ tools.ApprovalLedger = (*existingApproval)(nil)

// gatedText adapts ReloadGate to the telegram.GatedAction interface with
// honest, human-readable outcomes for every branch.
type gatedText struct{ g *ReloadGate }

func (gt gatedText) HandleText(ctx context.Context, reason string) string {
	out, outcome, err := gt.g.Handle(ctx, reason)
	switch {
	case err != nil:
		return "⚠️ reload_rules failed: " + err.Error()
	case outcome == tools.OutcomeApproved:
		return fmt.Sprintf("✅ reload approved & executed: %v", out)
	case outcome == tools.OutcomeDenied:
		return "🚫 reload denied — previous rules stay live."
	case outcome == tools.OutcomeTimeout:
		return "⏱️ approval timed out (10 min) — nothing executed. Rules unchanged."
	case outcome == tools.OutcomeExpired, outcome == tools.OutcomeShutdown:
		return "ℹ️ approval " + string(outcome) + " — nothing executed."
	}
	return "Unexpected outcome: " + string(outcome)
}

// GatedTextFor exposes the text adapter (tests + future transports).
func GatedTextFor(g *ReloadGate) telegram.GatedAction {
	return gatedText{g: g}
}
