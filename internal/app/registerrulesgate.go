package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
)

// RegisterRulesGate is the import twin of the export family (#60-#63):
// /rules export renders a live rule set as /addrule replay lines, and
// /register_rules replays those lines back into THIS box in one gated
// action. Moving a config between boxes becomes paste-one-message instead
// of per-rule /addrule typing (queue item #51, oldest open M).
//
// Semantics — strict on purpose (same philosophy as parseAddRule):
//   - EVERY line must parse as a valid /addrule spec BEFORE any approval
//     row exists; one bad line rejects the whole message (batch atomicity:
//     the human approves what they SEE, and a half-imported family breaks
//     the specific-before-general ordering).
//   - Only chat-addable tools (router.ChatAddable) — identical allowlist
//     to /addrule, Hard Rule 12 symmetry. Non-addable lines in an export
//     (reference list) are ignored, not errors: they document what ships
//     with the binary's Seeded().
//   - ONE approval for the whole batch; the reason carries the full line
//     list so the buttons decide exactly what will land.
//   - Run is idempotent upsert (ON CONFLICT name DO UPDATE) — replaying a
//     box's own export converges instead of duplicating.
type RegisterRulesGate struct {
	Store  *store.Store
	Router *router.Router
	// QuickTimeout overrides the wait for tests (0 = default 10 min).
	QuickTimeout time.Duration
}

// GateConfigFor mirrors AddRuleGate: humans answer on their phone.
func (g *RegisterRulesGate) GateConfig() tools.GateConfig {
	wait := 10 * time.Minute
	if g.QuickTimeout > 0 {
		wait = g.QuickTimeout
	}
	return tools.GateConfig{WaitTimeout: wait, PollInterval: 10 * time.Millisecond}
}

// maxRegisterLines bounds a batch — a pasted export larger than this is a
// paste error, not a config (bounded output rule #10's input-side twin).
const maxRegisterLines = 50

// parseRegisterRules extracts /addrule replay lines from the pasted text.
// Lines that are not /addrule lines (headers, reference lists, prose) are
// skipped; each remaining line is parsed strictly via parseAddRule. One
// invalid /addrule line fails the whole batch pre-approval.
func parseRegisterRules(text string) ([]*ruleSpec, error) {
	var specs []*ruleSpec
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "/addrule ") {
			continue // header/reference/prose — not replay material
		}
		if len(specs) >= maxRegisterLines {
			return nil, fmt.Errorf("too many rules in one batch (max %d)", maxRegisterLines)
		}
		spec, err := parseAddRule(line)
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("usage: /register_rules <pasted /rules export lines> — no /addrule lines found")
	}
	return specs, nil
}

// Request creates ONE approval row for the whole batch; the reason lists
// every line so the human approves exactly what lands.
func (g *RegisterRulesGate) Request(ctx context.Context, specs []*ruleSpec) (int64, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "register %d rule(s):", len(specs))
	for i, s := range specs {
		if i < 5 {
			fmt.Fprintf(&b, "\n• %s → %s", s.Name, s.Tool)
		}
	}
	if len(specs) > 5 {
		fmt.Fprintf(&b, "\n• … +%d more", len(specs)-5)
	}
	payload, err := tools.MarshalPayload(map[string]any{
		"action": "register_rules", "rules": len(specs),
	})
	if err != nil {
		return 0, err
	}
	return g.Store.CreateApproval(ctx, "register_rules", payload, b.String())
}

// Run upserts every rule then hot-swaps the router once — one atomic
// batch, same insert shape as AddRuleGate.Run.
func (g *RegisterRulesGate) Run(ctx context.Context, specs []*ruleSpec) (any, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, s := range specs {
		if !router.ChatAddable(s.Tool) {
			return nil, fmt.Errorf("register_rules: tool %q not addable from chat", s.Tool)
		}
		args := s.Args
		if args == "" {
			args = "{}"
		}
		if err := g.Store.Exec(ctx,
			`INSERT INTO rules (name, pattern, tool, args_template, origin, state, enabled, created_ts)
			 VALUES (?,?,?,?, 'manual', 'active', 1, ?)
			 ON CONFLICT(name) DO UPDATE SET pattern=excluded.pattern, tool=excluded.tool,
			   args_template=excluded.args_template, enabled=1`,
			s.Name, s.Pattern, s.Tool, args, now); err != nil {
			return nil, fmt.Errorf("register_rules: insert %q: %w", s.Name, err)
		}
	}
	defs, err := router.LoadRules(ctx, g.Store)
	if err != nil {
		return nil, fmt.Errorf("register_rules: reload: %w", err)
	}
	if err := g.Router.Swap(defs); err != nil {
		return nil, fmt.Errorf("register_rules: swap rejected (rules kept in table, fix and /reload_rules): %w", err)
	}
	return map[string]any{"registered": len(specs), "rules": len(defs)}, nil
}

// Handle is the full gated flow: parse all → one ledger row → wait →
// execute the batch.
func (g *RegisterRulesGate) Handle(ctx context.Context, text string) (any, tools.GateOutcome, error) {
	specs, err := parseRegisterRules(text)
	if err != nil {
		return nil, "", err // parse errors surface pre-approval, no row created
	}
	id, err := g.Request(ctx, specs)
	if err != nil {
		return nil, "", err
	}
	lv := &existingApproval{st: g.Store, id: id}
	run := func(ctx context.Context) (any, error) { return g.Run(ctx, specs) }
	reason := fmt.Sprintf("register %d rule(s)", len(specs))
	return tools.RunGated(ctx, lv, g.GateConfig(), "register_rules", "{}", reason, run)
}

// ReplayAddruleLines is the offline twin of Handle, used by the ctl admin
// path (`aegis ctl rules import`, #66): same strict parse, same allowlist,
// same idempotent upsert + Swap — but NO approval ledger row, because ctl
// is the owner-at-keyboard surface and the operator's Enter key IS the
// approval. Returns how many rules were imported and the resulting live
// rule count.
func (g *RegisterRulesGate) ReplayAddruleLines(ctx context.Context, text string) (imported, live int, err error) {
	specs, err := parseRegisterRules(text)
	if err != nil {
		return 0, 0, err // nothing written on a bad batch
	}
	out, err := g.Run(ctx, specs)
	if err != nil {
		return 0, 0, err
	}
	if m, ok := out.(map[string]any); ok {
		if v, ok := m["registered"].(int); ok {
			imported = v
		}
		if v, ok := m["rules"].(int); ok {
			live = v
		}
	}
	return imported, live, nil
}

// registerRulesText adapts RegisterRulesGate to telegram.GatedAction,
// mirroring addRuleText's honest outcome rendering.
type registerRulesText struct{ g *RegisterRulesGate }

func (r registerRulesText) HandleText(ctx context.Context, text string) string {
	out, outcome, err := r.g.Handle(ctx, text)
	switch {
	case err != nil:
		if strings.Contains(err.Error(), "usage:") || strings.Contains(err.Error(), "must be") ||
			strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "compile") ||
			strings.Contains(err.Error(), "line ") {
			return "⚠️ " + err.Error()
		}
		return "⚠️ register_rules failed: " + err.Error()
	case outcome == tools.OutcomeApproved:
		return fmt.Sprintf("✅ rules registered & live: %v", out)
	case outcome == tools.OutcomeDenied:
		return "🚫 register_rules denied — nothing changed."
	case outcome == tools.OutcomeTimeout:
		return "⏱ register_rules approval timed out — nothing changed. Re-send to retry."
	default:
		return fmt.Sprintf("register_rules outcome: %s (%v)", outcome, out)
	}
}
