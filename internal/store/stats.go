package store

import "context"

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

// Stats computes the snapshot with canned SELECTs over the audit trail,
// rules table, and fallback corpus.
func (s *Store) Stats(ctx context.Context) (*StatsSnapshot, error) {
	out := &StatsSnapshot{
		BySource:     map[string]int{},
		ByInterface:  map[string]int{},
		AvgLatencyMS: map[string]int64{},
		RulesByState: map[string]int{},
	}

	if err := s.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&out.TotalRuns); err != nil {
		return nil, err
	}
	rows, err := s.Query(ctx,
		`SELECT decision_source, COUNT(*), AVG(latency_ms) FROM audit_events GROUP BY decision_source`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src string
		var n int
		var avg float64
		if err := rows.Scan(&src, &n, &avg); err != nil {
			rows.Close()
			return nil, err
		}
		out.BySource[src] = n
		out.AvgLatencyMS[src] = int64(avg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if out.TotalRuns > 0 {
		out.DeflectionRate = float64(out.BySource[SourceRouter]) / float64(out.TotalRuns)
	}

	rows, err = s.Query(ctx, `SELECT interface, COUNT(*) FROM audit_events GROUP BY interface`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var iface string
		var n int
		if err := rows.Scan(&iface, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.ByInterface[iface] = n
	}
	rows.Close()

	rows, err = s.Query(ctx, `SELECT state, COUNT(*) FROM rules GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.RulesByState[state] = n
	}
	rows.Close()

	rows, err = s.Query(ctx,
		`SELECT normalized_prompt, COUNT(*) c FROM fallback_events
		 GROUP BY normalized_prompt ORDER BY c DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sc ShapeCount
		if err := rows.Scan(&sc.Shape, &sc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		out.TopFallbacks = append(out.TopFallbacks, sc)
	}
	rows.Close()
	return out, rows.Err()
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
	rows, err := s.Query(ctx,
		`SELECT ts, interface, decision_source, rule_id, model, latency_ms, outcome
		 FROM audit_events WHERE trace_id=? ORDER BY id`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditTrail
	for rows.Next() {
		var a AuditTrail
		var rule, model *string
		if err := rows.Scan(&a.TS, &a.Interface, &a.DecisionSource, &rule, &model, &a.LatencyMS, &a.Outcome); err != nil {
			return nil, err
		}
		if rule != nil {
			a.RuleID = *rule
		}
		if model != nil {
			a.Model = *model
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
