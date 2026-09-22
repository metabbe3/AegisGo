package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"aegisgo/internal/router"
	"aegisgo/internal/tools"
)

// ReloadDiffGate extends ReloadGate with a preview: the approval reason
// embeds a compact old→new rules diff so the human approves what they
// SEE, not a promise (owner trust rule 2026-09-22).
type ReloadDiffGate struct {
	*ReloadGate
}

// diffRules renders the change set between the live rule set and the
// one a reload would install. Lines are human words, no JSON:
//
//	+ new-rule (Pattern → tool)
//	− dropped-rule
//	~ changed-rule: pattern or tool moved
func diffRules(live, next []router.RuleDef) string {
	liveIdx := make(map[string]router.RuleDef, len(live))
	for _, r := range live {
		liveIdx[r.Name] = r
	}
	nextIdx := make(map[string]router.RuleDef, len(next))
	for _, r := range next {
		nextIdx[r.Name] = r
	}
	var added, removed, changed []string
	for name, r := range nextIdx {
		old, ok := liveIdx[name]
		if !ok {
			added = append(added, fmt.Sprintf("+ %s → %s", name, r.Tool))
			continue
		}
		if old.Pattern != r.Pattern || old.Tool != r.Tool || old.ArgsTemplate != r.ArgsTemplate {
			changed = append(changed, fmt.Sprintf("~ %s", name))
		}
	}
	for name := range liveIdx {
		if _, ok := nextIdx[name]; !ok {
			removed = append(removed, fmt.Sprintf("− %s", name))
		}
	}
	for _, s := range [][]string{added, removed, changed} {
		sort.Strings(s)
	}
	var lines []string
	lines = append(lines, added...)
	lines = append(lines, removed...)
	lines = append(lines, changed...)
	if len(lines) == 0 {
		return "no rule changes"
	}
	return strings.Join(lines, "\n")
}

// RequestWithDiff stores the diff in the approval reason so it rides
// the existing push → button → edit pipeline unchanged.
func (g *ReloadDiffGate) RequestWithDiff(ctx context.Context, reason string) (int64, error) {
	live := g.Router.RuleDefs()
	next, err := router.LoadRules(ctx, g.Store)
	if err != nil {
		return 0, fmt.Errorf("reload_rules: preview load: %w", err)
	}
	full := reason
	if d := diffRules(live, next); d != "" {
		full = reason + "\nRule changes:\n" + d
	}
	return g.ReloadGate.Request(ctx, full)
}

// Handle keeps the exact ReloadGate flow; only the request carries a
// preview now.
func (g *ReloadDiffGate) Handle(ctx context.Context, reason string) (any, tools.GateOutcome, error) {
	id, err := g.RequestWithDiff(ctx, reason)
	if err != nil {
		return nil, "", err
	}
	lv := &existingApproval{st: g.Store, id: id}
	return tools.RunGated(ctx, lv, g.GateConfig(), "reload_rules", "{}", reason, g.Run)
}
