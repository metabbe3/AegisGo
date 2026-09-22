package app_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/store"
)

// TestNotifierWiredInBuild proves Build() starts a working notifier: an
// approval created AFTER Build must produce a sendMessage call.
func TestNotifierWiredInBuild(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "t.db")
	fake := newFakeBotAPI(t)

	t.Setenv("AEGIS_LLM", "off")
	t.Setenv("AEGIS_DB_PATH", db)
	t.Setenv("AEGIS_TELEGRAM_TOKEN", "stub-token")
	t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL+"/bot")
	t.Setenv("AEGIS_TELEGRAM_CHATS", "4242")
	t.Setenv("AEGIS_TELEGRAM_MODE", "poll")
	cfg := config.Load()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	_, cleanup, err := app.Build(context.Background(), cfg, config.TierFast, store.IFaceREST, logger)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer cleanup()

	// Prime lands async; give it a beat.
	time.Sleep(300 * time.Millisecond)

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateApproval(context.Background(), "k", "{}", "wire test"); err != nil {
		t.Fatalf("create: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range fake.bodies("sendMessage") {
			if txt, _ := b["text"].(string); txt != "" {
				if len(txt) >= len("Approval needed") && indexOfStr(txt, "Approval needed") >= 0 {
					return // announced
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("notifier did not announce within 5s — wiring broken in Build")
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
