// Package miner turns the LLM fallback corpus into router rules. Repeated
// prompt shapes whose runs consistently used one derivable tool become
// shadow rules; the engine's tool-choice comparison then promotes or
// demotes them (see internal/engine). LLM spend falls as traffic grows —
// guarded so it can never fall by answering wrongly.
package miner

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
)

// derivableTools are the tools whose args the miner can synthesize from a
// prompt shape: each takes exactly one workspace path. system_command is
// deliberately NOT derivable (input must never reach argv) and sql_query's
// args are free-form — neither is ever auto-mined.
var derivableTools = map[string]bool{
	"read_csv":  true,
	"csv_stats": true,
	"read_doc":  true,
}

// dominance is the fraction of a cluster's runs that must agree on one tool
// before a rule is proposed.
const dominance = 0.85

// minedArgsTemplate is the args template every mined rule uses: the only
// derivable tools take exactly one workspace path, spliced from the single
// pattern capture.
const minedArgsTemplate = `{"path":"$1"}`

// Proposal is one candidate rule the miner produced.
type Proposal struct {
	Name         string
	Pattern      string
	Tool         string
	ArgsTemplate string
	ClusterSize  int
}

// Options configures one mining pass.
type Options struct {
	// Threshold is the minimum cluster size (fallback events per normalized
	// shape) before a rule is proposed.
	Threshold int
}

// Mine analyzes the fallback corpus and inserts shadow rules for eligible
// clusters. It never touches seeded rules and never inserts duplicates
// (same pattern => skip).
func Mine(ctx context.Context, st *store.Store, opts Options, logger *slog.Logger) ([]Proposal, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Threshold <= 0 {
		opts.Threshold = 20
	}

	clusters, err := st.Query(ctx,
		`SELECT normalized_prompt, COUNT(*) c FROM fallback_events
		 GROUP BY normalized_prompt HAVING c >= ? ORDER BY c DESC`, opts.Threshold)
	if err != nil {
		return nil, err
	}
	defer clusters.Close()

	type cluster struct {
		shape string
		count int
	}
	var eligible []cluster
	for clusters.Next() {
		var c cluster
		if err := clusters.Scan(&c.shape, &c.count); err != nil {
			return nil, err
		}
		eligible = append(eligible, c)
	}
	if err := clusters.Err(); err != nil {
		return nil, err
	}

	var proposals []Proposal
	for _, c := range eligible {
		tool, ok := dominantTool(ctx, st, c.shape, c.count)
		if !ok {
			continue
		}
		pattern, ok := synthesizePattern(c.shape)
		if !ok {
			continue
		}

		// Idempotent: an identical pattern means this shape is already
		// mined (or seeded).
		var exists int
		if err := st.QueryRow(ctx,
			`SELECT COUNT(*) FROM rules WHERE pattern=?`, pattern).Scan(&exists); err != nil {
			return nil, err
		}
		if exists > 0 {
			continue
		}

		name, err := st.NextMinedName(ctx)
		if err != nil {
			return nil, err
		}
		if err := st.Exec(ctx,
			`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts, state)
			 VALUES (?,?,?,?,?,1,?,?)`,
			name, pattern, tool, minedArgsTemplate, "mined",
			time.Now().UTC().Format(time.RFC3339Nano), router.RuleShadow); err != nil {
			return nil, err
		}
		p := Proposal{Name: name, Pattern: pattern, Tool: tool,
			ArgsTemplate: minedArgsTemplate, ClusterSize: c.count}
		proposals = append(proposals, p)
		logger.Info("mined shadow rule", "name", name, "tool", tool,
			"cluster", c.count, "pattern", pattern)
	}
	return proposals, nil
}

// dominantTool returns the single tool ≥dominance of the cluster's runs
// used, when that tool is derivable.
func dominantTool(ctx context.Context, st *store.Store, shape string, clusterSize int) (string, bool) {
	rows, err := st.Query(ctx,
		`SELECT tools_used, COUNT(*) c FROM fallback_events
		 WHERE normalized_prompt=? GROUP BY tools_used ORDER BY c DESC`, shape)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	var top string
	var topCount int
	for rows.Next() {
		var used sql.NullString
		var c int
		if err := rows.Scan(&used, &c); err != nil {
			return "", false
		}
		if top == "" && used.Valid {
			top, topCount = used.String, c
		}
	}
	if top == "" {
		return "", false
	}
	names := strings.Split(top, ",")
	if len(names) != 1 || !derivableTools[names[0]] {
		return "", false
	}
	if float64(topCount)/float64(clusterSize) < dominance {
		return "", false
	}
	return names[0], true
}

// synthesizePattern converts a normalized prompt shape into an anchored
// rule pattern: <path> → the single capture group, <n> and <q> →
// non-capturing slots, everything else literal. Requires exactly one
// <path> — the arg we can synthesize.
func synthesizePattern(shape string) (string, bool) {
	fields := strings.Fields(shape)
	parts := make([]string, 0, len(fields))
	paths := 0
	for _, f := range fields {
		switch f {
		case "<path>":
			parts = append(parts, `(\S+)`)
			paths++
		case "<n>":
			parts = append(parts, `\d+`)
		case "<q>":
			parts = append(parts, `"[^"]*"`)
		default:
			parts = append(parts, regexp.QuoteMeta(f))
		}
	}
	if paths != 1 {
		return "", false
	}
	return strings.Join(parts, `\s+`), true
}

// Start runs Mine periodically (interval <= 0 disables). The returned stop
// function ends the loop and is idempotent — cleanup closures fire from
// more than one exit path, and a bare close(done) would panic on the second
// call (same contract as store.Close). The loop also exits when ctx is
// canceled, so a canceled boot context leaves no goroutine mining against
// a store the caller is about to close. Shadow evaluation and hot reload
// pick new rules up on their own cadences — mining never touches the live
// router.
func Start(ctx context.Context, st *store.Store, opts Options, interval time.Duration, logger *slog.Logger) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// Per-tick timeout is deliberately independent of ctx: the
				// boot ctx outlives many ticks, but one slow pass must never
				// outlive its own minute.
				mctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				if _, err := Mine(mctx, st, opts, logger); err != nil {
					logger.Error("mining pass failed", "error", err)
				}
				cancel()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() { stopOnce.Do(func() { close(done) }) }
}
