// Package logwatch — store.go: watch definitions live in SQLite so they
// survive restarts and can be added/removed at runtime WITHOUT rebuilding
// the binary (owner directive 28 Sep: "add/remove watchdog tanpa rebuild,
// flexible"). The v7 migration itself lives in internal/store (no import
// cycle: store owns every schema it runs).
package logwatch

import (
	"context"
	"time"
)

// StoredWatch is a watch spec row.
type StoredWatch struct {
	Name     string        `json:"name"`
	Path     string        `json:"path"`
	Pattern  string        `json:"pattern"`
	Every    time.Duration `json:"every_ms"`
	Cooldown time.Duration `json:"cooldown_ms"`
}

// SQLite store adapter — avoids importing internal/store here (the
// manager stays store-agnostic; app wires them).
type StoreAdapter struct {
	// Exec runs a statement.
	Exec func(ctx context.Context, query string, args ...any) error
	// QueryRows runs a query and yields rows as raw maps.
	QueryRows func(ctx context.Context, query string, args ...any) ([]map[string]any, error)
}

// SaveWatch upserts a watch definition.
func (a *StoreAdapter) SaveWatch(ctx context.Context, w StoredWatch) error {
	return a.Exec(ctx,
		`INSERT INTO log_watches (name, path, pattern, every_ms, cooldown_ms)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET path=excluded.path, pattern=excluded.pattern,
		   every_ms=excluded.every_ms, cooldown_ms=excluded.cooldown_ms`,
		w.Name, w.Path, w.Pattern, int64(w.Every/time.Millisecond), int64(w.Cooldown/time.Millisecond))
}

// DeleteWatch removes a definition.
func (a *StoreAdapter) DeleteWatch(ctx context.Context, name string) error {
	return a.Exec(ctx, `DELETE FROM log_watches WHERE name=?`, name)
}

// ListWatches reads every stored definition.
func (a *StoreAdapter) ListWatches(ctx context.Context) ([]StoredWatch, error) {
	rows, err := a.QueryRows(ctx, `SELECT name, path, pattern, every_ms, cooldown_ms FROM log_watches ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := make([]StoredWatch, 0, len(rows))
	for _, r := range rows {
		sw := StoredWatch{
			Name:    asString(r["name"]),
			Path:    asString(r["path"]),
			Pattern: asString(r["pattern"]),
		}
		if v := asInt(r["every_ms"]); v > 0 {
			sw.Every = time.Duration(v) * time.Millisecond
		}
		if v := asInt(r["cooldown_ms"]); v != 0 {
			sw.Cooldown = time.Duration(v) * time.Millisecond
		}
		out = append(out, sw)
	}
	return out, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
