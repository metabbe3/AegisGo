package router

import (
	"context"
	"fmt"
	"time"
)

// ReconcileSeeded backfills rule definitions into the rules table with
// INSERT OR IGNORE so rules added to Seeded() in newer builds reach DBs
// created by older builds — without touching any row an operator already
// edited or disabled. It reports the names actually inserted and the seed
// names that exist but are NOT live (shadowed/demoted/disabled): an
// operator decision the reconcile must surface, never silently revert
// (INSERT OR IGNORE guarantees it cannot).
//
// The DB is the source of truth after first boot; this helper is the
// migration path for "the code grew a rule the old DB never saw"
// (prod DBs do not re-seed — see lessons 2026-09-28).
func ReconcileSeeded(ctx context.Context, q queryer, defs []RuleDef) (inserted []string, shadowed []string, err error) {
	if len(defs) == 0 {
		defs = Seeded()
	}
	existing, err := existingRuleRows(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range defs {
		row, ok := existing[d.Name]
		if !ok {
			if err := q.Exec(ctx,
				`INSERT OR IGNORE INTO rules (name, pattern, tool, args_template, origin, state, enabled, created_ts)
				 VALUES (?,?,?,?,?,?,?,?)`,
				d.Name, d.Pattern, d.Tool, d.ArgsTemplate, d.Origin, d.State,
				1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return inserted, shadowed, fmt.Errorf("reconcile seeded: %s: %w", d.Name, err)
			}
			// OR IGNORE raced with a concurrent insert → still not ours to report.
			inserted = append(inserted, d.Name)
			continue
		}
		if row.state != RuleActive || !row.enabled {
			shadowed = append(shadowed, d.Name)
		}
	}
	return inserted, shadowed, nil
}

// ruleRow is the minimal live-ness projection of a rules row.
type ruleRow struct {
	state   string
	enabled bool
}

// existingRuleRows reads name → {state, enabled} for every rules row.
func existingRuleRows(ctx context.Context, q queryer) (map[string]ruleRow, error) {
	rows, err := q.Query(ctx, `SELECT name, state, enabled FROM rules`)
	if err != nil {
		return nil, fmt.Errorf("reconcile seeded: reading rules: %w", err)
	}
	defer rows.Close()
	out := map[string]ruleRow{}
	for rows.Next() {
		var (
			name    string
			state   string
			enabled int
		)
		if err := rows.Scan(&name, &state, &enabled); err != nil {
			return nil, fmt.Errorf("reconcile seeded: scanning rules: %w", err)
		}
		out[name] = ruleRow{state: state, enabled: enabled != 0}
	}
	return out, rows.Err()
}
