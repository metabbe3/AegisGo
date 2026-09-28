package tools

import (
	"testing"
	"time"
)

func TestDefaultGateConfig(t *testing.T) {
	cfg := DefaultGateConfig()
	if cfg.WaitTimeout != 2*time.Minute {
		t.Errorf("WaitTimeout = %v, want 2m", cfg.WaitTimeout)
	}
	if cfg.PollInterval != time.Second {
		t.Errorf("PollInterval = %v, want 1s", cfg.PollInterval)
	}
}

func TestCancelJobNotRunning(t *testing.T) {
	m := NewJobManager()
	// cancelling an unknown job: ok=false, exists=false — typed, no panic
	if _, ok, exists := m.CancelJob("nope"); ok || exists {
		t.Errorf("CancelJob(unknown) = ok=%v exists=%v, want false/false", ok, exists)
	}
}

func TestCatalogCommandsNonEmpty(t *testing.T) {
	cc := CatalogCommands()
	if len(cc) == 0 {
		t.Fatal("catalog should not be empty")
	}
	for k, desc := range cc {
		if k == "" || desc == "" {
			t.Errorf("catalog entry %q has empty key/description", k)
		}
	}
}

func TestClampStringBounded(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := clampString(string(long), 200)
	if len(got) > 203 { // 200 + multi-byte ellipsis
		t.Errorf("clampString len = %d, want <= 203", len(got))
	}
	if clampString("short", 200) != "short" {
		t.Error("short string should pass through unchanged")
	}
}
