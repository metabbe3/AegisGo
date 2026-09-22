package router

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"aegisgo/internal/logx"
	"aegisgo/internal/loop"
)

// rulesstore: the rules table is the hot-reloadable source of truth. On
// first boot it is seeded from code; afterwards, editing rows (sqlite3 CLI,
// admin tooling, or the Phase-3 miner) changes routing within one reload
// interval — no restart, no redeploy.

// LoadRules reads enabled rules from the store's rules table, seeding it on
// first boot.
func LoadRules(ctx context.Context, q queryer) ([]RuleDef, error) {
	rows, err := q.Query(ctx,
		`SELECT name, pattern, tool, args_template, origin, state FROM rules WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var defs []RuleDef
	for rows.Next() {
		var d RuleDef
		if err := rows.Scan(&d.Name, &d.Pattern, &d.Tool, &d.ArgsTemplate, &d.Origin, &d.State); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		if err := seedRules(ctx, q); err != nil {
			return nil, err
		}
		defs = Seeded()
		for i := range defs {
			defs[i].State = RuleActive
		}
		return defs, nil
	}
	return defs, nil
}

// seedRules inserts the code-level seed set into an empty rules table.
func seedRules(ctx context.Context, q queryer) error {
	for _, d := range Seeded() {
		if err := q.Exec(ctx,
			`INSERT OR IGNORE INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts)
			 VALUES (?,?,?,?,?,1,?)`,
			d.Name, d.Pattern, d.Tool, d.ArgsTemplate, d.Origin,
			time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

// queryer is the database surface LoadRules needs (*store.Store and tests'
// fakes both satisfy it).
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	Exec(ctx context.Context, query string, args ...any) error
}

// StartHotReload periodically re-reads the rules table and swaps the live
// set (every tick bounded to 5s; reloadEvery <= 0 disables it). The
// returned stop is idempotent, and the loop also exits when ctx is
// canceled. A bad row set keeps the previous rules (log, don't crash).
func StartHotReload(ctx context.Context, r *Router, q queryer, reloadEvery time.Duration, logger *slog.Logger) (stop func()) {
	logger = logx.Or(logger)
	return loop.Periodic(ctx, reloadEvery, 5*time.Second, func(ctx context.Context) {
		defs, err := LoadRules(ctx, q)
		if err != nil {
			logger.Error("rules reload failed; keeping previous rules", "error", err)
			return
		}
		if err := r.Swap(defs); err != nil {
			logger.Error("rules swap rejected; keeping previous rules", "error", err)
			return
		}
		logger.Debug("rules reloaded", "count", len(defs))
	})
}
