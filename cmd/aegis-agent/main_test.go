// Tests for the aegis-agent command shell. run and repl take injected
// stdout/stdin, so every test drives the real offline pipeline (temp store →
// seeded router → system_command) the same way the binary does — only the
// terminal is faked. AEGIS_LLM=off keeps the provider out entirely.
package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/store"
)

// baseEnv applies the offline baseline every test boots under: LLM kill
// switch on (no credentials anywhere), temp store, temp workspace, background
// loops off, no external interfaces. config.Load treats "" exactly like
// unset, so blanking plus t.Setenv's auto-restore keeps tests hermetic against
// the developer's shell. Returns the store path for post-run inspection.
func baseEnv(t *testing.T) string {
	t.Helper()
	for _, k := range []string{
		"AEGIS_MCP_SERVERS", "AEGIS_TELEGRAM_TOKEN", "AEGIS_GRPC_ADDR",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AEGIS_LLM", "off")
	dbPath := filepath.Join(t.TempDir(), "t.db")
	t.Setenv("AEGIS_DB_PATH", dbPath)
	t.Setenv("AEGIS_WORKSPACE", t.TempDir())
	t.Setenv("AEGIS_RULES_RELOAD", "0")
	t.Setenv("AEGIS_MINER_INTERVAL", "0")
	return dbPath
}

// buildEngine boots the same app run() boots (quietly) and hands back the
// engine for direct repl tests. The returned cleanup closes the store,
// draining queued audit writes.
func buildEngine(t *testing.T) (*engine.Engine, func()) {
	t.Helper()
	a, cleanup, err := app.Build(context.Background(), config.Load(), config.TierSmart,
		store.IFaceCLI, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("building app: %v", err)
	}
	return a.Engine, cleanup
}

// TestRunOneShotRouterHit: a prompt argument answers via the seeded router,
// prints only the answer (no decision header — that is the REPL shape), and
// leaves exactly one regex_router audit row keyed by the rule.
func TestRunOneShotRouterHit(t *testing.T) {
	dbPath := baseEnv(t)

	var buf bytes.Buffer
	if err := run(context.Background(), "smart", []string{"/hostname"}, &buf); err != nil {
		t.Fatalf("run one-shot: %v", err)
	}
	out := buf.String()
	hn, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	if !strings.Contains(out, hn) {
		t.Errorf("one-shot output %q, want it to contain the hostname %q", out, hn)
	}
	if !strings.Contains(out, `"command": "hostname"`) {
		t.Errorf("one-shot output %q, want the system_command answer payload", out)
	}
	if strings.Contains(out, "regex_router") {
		t.Errorf("one-shot output %q must not carry the REPL decision header", out)
	}

	// run's deferred cleanup drains the async batcher, so the audit row is
	// durable by the time run returns; verify from a fresh connection.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st.Close()
	var n int
	if err := st.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE rule_id='hostname' AND decision_source='regex_router'`).
		Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit rows for /hostname = %d, want 1", n)
	}
}

func TestRunBadTier(t *testing.T) {
	baseEnv(t)
	var buf bytes.Buffer
	err := run(context.Background(), "bogus", []string{"/uptime"}, &buf)
	if err == nil || !strings.Contains(err.Error(), `unknown tier "bogus"`) {
		t.Fatalf("run with bogus tier error = %v, want unknown-tier failure", err)
	}
}

// TestREPLExitAndQuit: both exit words leave the loop after the first prompt
// without ever consulting the engine.
func TestREPLExitAndQuit(t *testing.T) {
	baseEnv(t)
	eng, cleanup := buildEngine(t)
	defer cleanup()

	for _, word := range []string{"exit", "quit"} {
		var buf bytes.Buffer
		if err := repl(context.Background(), eng, strings.NewReader(word+"\n"), &buf); err != nil {
			t.Fatalf("repl on %q: %v", word, err)
		}
		out := buf.String()
		if !strings.Contains(out, "aegis-agent interactive mode") {
			t.Errorf("repl on %q: output %q, want the banner", word, out)
		}
		if !strings.Contains(out, "\n> ") {
			t.Errorf("repl on %q: output %q, want the prompt", word, out)
		}
		if strings.Contains(out, "regex_router") {
			t.Errorf("repl on %q ran the engine; output %q", word, out)
		}
	}
}

func TestREPLEOF(t *testing.T) {
	baseEnv(t)
	eng, cleanup := buildEngine(t)
	defer cleanup()

	var buf bytes.Buffer
	if err := repl(context.Background(), eng, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("repl on EOF: %v", err)
	}
	// EOF prints the prompt once, then the newline Ctrl-D leaves behind.
	if want := "\n> \n"; !strings.HasSuffix(buf.String(), want) {
		t.Errorf("repl on EOF output = %q, want it to end with %q", buf.String(), want)
	}
}

// TestREPLPromptThenBlank: a router command answers with its decision header
// (source, rule, latency) and the answer; the following blank line exits.
func TestREPLPromptThenBlank(t *testing.T) {
	baseEnv(t)
	eng, cleanup := buildEngine(t)
	defer cleanup()

	var buf bytes.Buffer
	if err := repl(context.Background(), eng, strings.NewReader("/hostname\n\n"), &buf); err != nil {
		t.Fatalf("repl: %v", err)
	}
	out := buf.String()
	i := strings.Index(out, "[regex_router via hostname,")
	if i < 0 {
		t.Fatalf("repl output %q, want a [regex_router via hostname, …] header", out)
	}
	// The line after the decision header is the answer; it must be non-empty.
	rest := strings.SplitN(out[i:], "\n", 3)
	if len(rest) < 2 || strings.TrimSpace(rest[1]) == "" {
		t.Errorf("repl output %q, want a non-empty answer after the header", out)
	}
}

// TestREPLCancelledContext pins the loop's cancellation contract as written:
// a pre-cancelled context still reads and runs the FIRST prompt (the ctx is
// only checked after the run), then returns nil — no second prompt.
func TestREPLCancelledContext(t *testing.T) {
	baseEnv(t)
	eng, cleanup := buildEngine(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := repl(ctx, eng, strings.NewReader("/hostname\n/kernel\n"), &buf); err != nil {
		t.Fatalf("repl with cancelled ctx: %v", err)
	}
	out := buf.String()
	// The run itself fails fast (exec under a done ctx) but is still reported
	// as a router decision per Hard Rule 6.
	if !strings.Contains(out, "[regex_router via hostname,") {
		t.Errorf("repl output %q, want the first prompt's decision header", out)
	}
	if n := strings.Count(out, "\n> "); n != 1 {
		t.Errorf("prompts printed = %d, want exactly 1 (ctx check exits after the first run)", n)
	}
}
