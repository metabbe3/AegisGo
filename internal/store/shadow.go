package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Rule lifecycle states mirror internal/router's; duplicated as strings to
// keep store free of router imports (the values ARE the contract).
const (
	RuleStateActive  = "active"
	RuleStateShadow  = "shadow"
	RuleStateDemoted = "demoted"
)

// ShadowEvent is one tool-choice comparison between a rule and the LLM.
type ShadowEvent struct {
	RuleName string
	TraceID  string
	Agreed   bool
	LLMTools []string
}

// RecordShadow persists one comparison. Synchronous on purpose: the
// engine reads the streak back immediately to decide promotion, and a
// lagging write would delay (or worse, miss) a promotion boundary. One
// row per LLM run — not a hot path.
func (s *Store) RecordShadow(ctx context.Context, ev ShadowEvent) error {
	return s.Exec(ctx, `INSERT INTO shadow_events (ts, rule_name, trace_id, agreed, llm_tools)
		VALUES (?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		ev.RuleName, ev.TraceID, boolToInt(ev.Agreed),
		nullable(strings.Join(ev.LLMTools, ",")),
	)
}

// ShadowStreak counts consecutive agreements for a rule, most recent first,
// stopping at the first disagreement. The promotion signal.
func (s *Store) ShadowStreak(ctx context.Context, rule string) (int, error) {
	rows, err := s.Query(ctx,
		`SELECT agreed FROM shadow_events WHERE rule_name=? ORDER BY id DESC LIMIT 100`, rule)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	streak := 0
	for rows.Next() {
		var agreed int
		if err := rows.Scan(&agreed); err != nil {
			return 0, err
		}
		if agreed == 0 {
			break
		}
		streak++
	}
	return streak, rows.Err()
}

// SetRuleState transitions a rule's lifecycle state and enabled flag.
func (s *Store) SetRuleState(ctx context.Context, rule, state string, enabled bool) error {
	return s.Exec(ctx, `UPDATE rules SET state=?, enabled=? WHERE name=?`,
		state, boolToInt(enabled), rule)
}

// RuleStates lists every rule with its state (`aegis ctl rules list`).
func (s *Store) RuleStates(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.Query(ctx,
		`SELECT name, pattern, tool, origin, state, enabled FROM rules ORDER BY origin, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var name, pattern, tool, origin, state string
		var enabled int
		if err := rows.Scan(&name, &pattern, &tool, &origin, &state, &enabled); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"name": name, "pattern": pattern, "tool": tool,
			"origin": origin, "state": state, "enabled": enabled == 1,
		})
	}
	return out, rows.Err()
}

// NextMinedName returns an unused mined-rule name.
func (s *Store) NextMinedName(ctx context.Context) (string, error) {
	var n int
	if err := s.QueryRow(ctx, `SELECT COUNT(*) FROM rules WHERE origin='mined'`).Scan(&n); err != nil {
		return "", err
	}
	for {
		n++
		name := fmt.Sprintf("mined_%d", n)
		var exists int
		if err := s.QueryRow(ctx, `SELECT COUNT(*) FROM rules WHERE name=?`, name).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return name, nil
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
