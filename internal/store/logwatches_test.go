package store

import (
	"context"
	"testing"
	"time"

	"aegisgo/internal/logwatch"
)

// The v7 log_watches round-trip against a REAL SQLite — pins schema,
// upsert, delete, and the duration ms encoding together.
func TestLogWatchesRoundTrip(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &logwatch.StoreAdapter{
		Exec: func(ctx context.Context, q string, args ...any) error { return st.Exec(ctx, q, args...) },
		QueryRows: func(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
			return st.QueryMaps(ctx, q, args...)
		},
	}
	ctx := context.Background()

	w := logwatch.StoredWatch{Name: "gw", Path: "/tmp/gw.log", Pattern: "panic|FATAL",
		Every: 30 * time.Second, Cooldown: 10 * time.Minute}
	if err := a.SaveWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := a.ListWatches(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	if got[0].Pattern != "panic|FATAL" || got[0].Every != 30*time.Second {
		t.Fatalf("round-trip = %+v", got[0])
	}
	// upsert
	w.Pattern = "changed"
	if err := a.SaveWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, _ = a.ListWatches(ctx)
	if len(got) != 1 || got[0].Pattern != "changed" {
		t.Fatalf("upsert = %+v", got)
	}
	// delete
	if err := a.DeleteWatch(ctx, "gw"); err != nil {
		t.Fatal(err)
	}
	got, _ = a.ListWatches(ctx)
	if len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
}
