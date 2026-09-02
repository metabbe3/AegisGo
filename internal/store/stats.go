package store

import (
	"context"
	"database/sql"
)

// StatsSnapshot is the observability surface shared by GET /v1/stats and
// `aegis ctl stats`. DeflectionRate is the headline metric: the fraction of
// runs answered by the deterministic router at zero LLM cost.
type StatsSnapshot struct {
	TotalRuns      int              `json:"total_runs"`
	BySource       map[string]int   `json:"by_decision_source"`
	DeflectionRate float64          `json:"deflection_rate"`
	ByInterface    map[string]int   `json:"by_interface"`
	AvgLatencyMS   map[string]int64 `json:"avg_latency_ms_by_source"`
	RulesByState   map[string]int   `json:"rules_by_state"`
	TopFallbacks   []ShapeCount     `json:"top_fallback_shapes"`
}

// ShapeCount is one fallback shape and its frequency (mining preview).
type ShapeCount struct {
	Shape string `json:"shape"`
	Count int    `json:"count"`
}

// fallbackShapeSQL is the fallback-corpus grouping stats and the miner
// share: stats previews the top shapes, the miner clusters them for rule
// proposals — same GROUP BY, different trailing filter (FallbackShapes
// owns the assembly).
const fallbackShapeSQL = `SELECT normalized_prompt, COUNT(*) c FROM fallback_events GROUP BY normalized_prompt`

// groupCount is one GROUP BY row: a label and its count.
type groupCount struct {
	key string
	n   int
}

// FallbackShapes lists fallback-corpus shapes with their counts, largest
// first, keeping only shapes seen at least minCount times and at most limit
// rows (0 = no limit) — the one query behind the stats preview and the
// miner's clustering. A GROUP BY count is never below 1, so minCount=1 is
// the unfiltered case.
func (s *Store) FallbackShapes(ctx context.Context, minCount, limit int) ([]ShapeCount, error) {
	q := fallbackShapeSQL + ` HAVING c >= ? ORDER BY c DESC`
	args := []any{minCount}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return QueryAll(ctx, s, q, func(r *sql.Rows) (ShapeCount, error) {
		var v ShapeCount
		return v, r.Scan(&v.Shape, &v.Count)
	}, args...)
}

// counts runs a two-column GROUP BY (label, COUNT(*)) into a map — the
// shared shape of the by-interface and rules-by-state aggregations.
func (s *Store) counts(ctx context.Context, query string, args ...any) (map[string]int, error) {
	rows, err := QueryAll(ctx, s, query, func(r *sql.Rows) (groupCount, error) {
		var v groupCount
		return v, r.Scan(&v.key, &v.n)
	}, args...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, v := range rows {
		out[v.key] = v.n
	}
	return out, nil
}

// Stats computes the snapshot with canned SELECTs over the audit trail,
// rules table, and fallback corpus.
func (s *Store) Stats(ctx context.Context) (*StatsSnapshot, error) {
	out := &StatsSnapshot{
		BySource:     map[string]int{},
		AvgLatencyMS: map[string]int64{},
	}

	if err := s.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&out.TotalRuns); err != nil {
		return nil, err
	}
	type srcAgg struct {
		src string
		n   int
		avg float64
	}
	srcRows, err := QueryAll(ctx, s,
		`SELECT decision_source, COUNT(*), AVG(latency_ms) FROM audit_events GROUP BY decision_source`,
		func(r *sql.Rows) (srcAgg, error) {
			var v srcAgg
			return v, r.Scan(&v.src, &v.n, &v.avg)
		})
	if err != nil {
		return nil, err
	}
	for _, v := range srcRows {
		out.BySource[v.src] = v.n
		out.AvgLatencyMS[v.src] = int64(v.avg)
	}

	if out.TotalRuns > 0 {
		out.DeflectionRate = float64(out.BySource[SourceRouter]) / float64(out.TotalRuns)
	}

	out.ByInterface, err = s.counts(ctx, `SELECT interface, COUNT(*) FROM audit_events GROUP BY interface`)
	if err != nil {
		return nil, err
	}
	out.RulesByState, err = s.counts(ctx, `SELECT state, COUNT(*) FROM rules GROUP BY state`)
	if err != nil {
		return nil, err
	}

	out.TopFallbacks, err = s.FallbackShapes(ctx, 1, 10)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AuditTrail lists a trace's audit rows chronologically (replay).
type AuditTrail struct {
	TS             string `json:"ts"`
	Interface      string `json:"interface"`
	DecisionSource string `json:"decision_source"`
	RuleID         string `json:"rule_id"`
	Model          string `json:"model"`
	LatencyMS      int64  `json:"latency_ms"`
	Outcome        string `json:"outcome"`
}

// Replay returns the audit rows for a trace id.
func (s *Store) Replay(ctx context.Context, traceID string) ([]AuditTrail, error) {
	return QueryAll(ctx, s,
		`SELECT ts, interface, decision_source, rule_id, model, latency_ms, outcome
		 FROM audit_events WHERE trace_id=? ORDER BY id`,
		func(r *sql.Rows) (AuditTrail, error) {
			var a AuditTrail
			var rule, model *string
			if err := r.Scan(&a.TS, &a.Interface, &a.DecisionSource, &rule, &model, &a.LatencyMS, &a.Outcome); err != nil {
				return AuditTrail{}, err
			}
			if rule != nil {
				a.RuleID = *rule
			}
			if model != nil {
				a.Model = *model
			}
			return a, nil
		}, traceID)
}
