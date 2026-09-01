package router

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// rulesstore: the rules table is the hot-reloadable source of truth. On
// first boot it is seeded from code; afterwards, editing rows (sqlite3 CLI,
// admin tooling, or the Phase-3 miner) changes routing within one reload
// interval — no restart, no redeploy.

// LoadRules reads enabled rules from the store's rules table, seeding it on
// first boot.
func LoadRules(ctx context.Context, q queryer) ([]RuleDef, error) {
	rows, err := q.Query(ctx,
		`SELECT name, pattern, tool, args_template, origin FROM rules WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var defs []RuleDef
	for rows.Next() {
		var d RuleDef
		if err := rows.Scan(&d.Name, &d.Pattern, &d.Tool, &d.ArgsTemplate, &d.Origin); err != nil {
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
		return Seeded(), nil
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
// set. reloadEvery <= 0 disables it. The returned stop function ends the
// loop. A bad row set keeps the previous rules (log, don't crash).
func StartHotReload(r *Router, q queryer, reloadEvery time.Duration, logger *slog.Logger) (stop func()) {
	if reloadEvery <= 0 || logger == nil {
		logger = slog.Default()
	}
	if reloadEvery <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(reloadEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defs, err := LoadRules(ctx, q)
				cancel()
				if err != nil {
					logger.Error("rules reload failed; keeping previous rules", "error", err)
					continue
				}
				if err := r.Swap(defs); err != nil {
					logger.Error("rules swap rejected; keeping previous rules", "error", err)
					continue
				}
				logger.Debug("rules reloaded", "count", len(defs))
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
