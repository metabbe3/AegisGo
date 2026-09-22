package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestNewAppliesLevelAndFormat: level filters records and json selects the
// handler — the two knobs AEGIS_LOG_LEVEL and the serve/agent split expose.
// Probed with Debug: on only when the level is debug.
func TestNewAppliesLevelAndFormat(t *testing.T) {
	cases := []struct {
		level  string
		wantOn bool
	}{
		{"debug", true},
		{"DEBUG", true}, // case-insensitive
		{"info", false}, // info default swallows debug
		{"warn", false},
		{"error", false},
		{"", false},        // blank: info default
		{"verbose", false}, // unrecognized: info default, never a parse error
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		l := New(tc.level, &buf, false)
		l.Debug("hello")
		if on := strings.Contains(buf.String(), "hello"); on != tc.wantOn {
			t.Errorf("New(%q).Debug logged=%v, want %v (buf %q)", tc.level, on, tc.wantOn, buf.String())
		}
	}
}

// TestNewJSONHandlerOutput: serve-mode loggers emit one JSON object per
// line with the level and message as fields.
func TestNewJSONHandlerOutput(t *testing.T) {
	var buf bytes.Buffer
	New("info", &buf, true).Info("serving", "addr", ":8080")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "serving" || rec["level"] != "INFO" || rec["addr"] != ":8080" {
		t.Errorf("record = %v, want msg=serving level=INFO addr=:8080", rec)
	}
}

// TestOrNilFallsBackToDefault: Or(nil) is slog.Default; non-nil passes
// through unchanged.
func TestOrNilFallsBackToDefault(t *testing.T) {
	if got := Or(nil); got != slog.Default() {
		t.Error("Or(nil) != slog.Default()")
	}
	l := slog.Default()
	if got := Or(l); got != l {
		t.Error("Or(l) != l for non-nil l")
	}
}
