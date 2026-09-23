package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aegisgo/internal/store"
)

// TestBackupOnceCreatesSidecarAndPrunes: two runs on distinct dates keep
// only the newest retention files; the sidecar is a readable SQLite DB.
func TestBackupOnceCreatesSidecarAndPrunes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	st.Audit(context.Background(), store.AuditEvent{TraceID: "t1", Interface: "test", Prompt: "seed", DecisionSource: "test", Outcome: "ok"})
	if err := st.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	fake := nopLogger{}
	if err := backupOnce(context.Background(), st, dbPath, fake); err != nil {
		t.Fatalf("backupOnce: %v", err)
	}
	// Forge 9 dated sidecars, rerun prune path via a second backup call.
	for i := 0; i < 9; i++ {
		n := filepath.Join(dir, "app.db.backup-2026090"+string(rune('0'+i))+".sidecar")
		if err := os.WriteFile(n, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := backupOnce(context.Background(), st, dbPath, fake); err != nil {
		t.Fatalf("backupOnce 2: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "app.db.backup-*.sidecar"))
	if len(matches) != 7 {
		t.Fatalf("retention: want 7 sidecars, got %d (%v)", len(matches), matches)
	}
	// The newest sidecar must open as a real SQLite database (VACUUM INTO
	// artifact, not a stub).
	ro, err := store.Open(matches[len(matches)-1])
	if err != nil {
		t.Fatalf("sidecar not openable: %v", err)
	}
	ro.Close()
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
