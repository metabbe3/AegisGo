package logwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerSpecsAndRemove(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	if err := os.WriteFile(p, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(4)
	defer m.Stop()
	if err := m.Add(context.Background(), Watch{Name: "aa", Path: p, Pattern: "x", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	specs := m.Specs()
	if len(specs) != 1 || specs[0].Name != "aa" {
		t.Fatalf("specs = %+v", specs)
	}
	if !m.Remove("aa") {
		t.Fatal("Remove existing = false")
	}
	if m.Remove("aa") {
		t.Fatal("Remove twice = true")
	}
	if len(m.Specs()) != 0 {
		t.Fatal("specs after remove not empty")
	}
}

// fakeRowsAdapter exercises the adapter's SQL against an in-memory fake
// (no store import — cycle): the map conversion logic (asString/asInt,
// defaults-on-read) is what matters here.
func TestAdapterRowConversion(t *testing.T) {
	a := &StoreAdapter{
		Exec: func(ctx context.Context, q string, args ...any) error { return nil },
		QueryRows: func(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "w1", "path": "/a", "pattern": "p", "every_ms": int64(30000), "cooldown_ms": int64(600000)},
				{"name": "w2", "path": "/b", "pattern": "q", "every_ms": float64(0), "cooldown_ms": float64(0)},
			}, nil
		},
	}
	got, err := a.ListWatches(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	if got[0].Every != 30*time.Second || got[0].Cooldown != 10*time.Minute {
		t.Fatalf("durations = %v/%v", got[0].Every, got[0].Cooldown)
	}
	if got[1].Every != 0 || got[1].Cooldown != 0 {
		t.Fatalf("zero row = %v/%v (must stay 0=unset)", got[1].Every, got[1].Cooldown)
	}
}
