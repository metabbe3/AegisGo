// Package app tests: Build is the integration seam where every subsystem
// meets, so each test here boots the real offline pipeline — temp SQLite
// store → builtin tools → registry → DB-backed router — with no fakes in
// the middle. AEGIS_LLM=off keeps the provider out entirely except where a
// test explicitly constructs one (construction does no I/O; the provider is
// never run). Telegram tests ride a loopback fake Bot API, covering both
// transports plus the webhook delivery path end to end.
package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/server"
	"aegisgo/internal/store"
	"aegisgo/internal/telegram"
	"aegisgo/internal/trace"
)

// appEnvKeys lists every environment variable config.Load reads (mirroring
// internal/config's own hermetic-env approach, defined locally here so the
// two test files stay decoupled). Load treats "" exactly like unset — envOr,
// AtoiDefault, and the list parsers all fall back on empty input — so
// blanking the full list plus t.Setenv's auto-restore keeps every test
// hermetic against a developer's shell.
var appEnvKeys = []string{
	"AEGIS_PROVIDER", "AEGIS_MODEL", "AEGIS_MODEL_FAST", "AEGIS_MODEL_SMART",
	"AEGIS_INSTRUCTIONS", "OPENAI_API_KEY", "OPENAI_BASE_URL",
	"ANTHROPIC_API_KEY", "FOUNDRY_ENDPOINT", "AEGIS_WORKSPACE",
	"AEGIS_MCP_SERVERS", "AEGIS_ADDR", "AEGIS_GRPC_ADDR", "AEGIS_LOG_LEVEL",
	"AEGIS_LLM", "AEGIS_DB_PATH", "AEGIS_SQL_DSN", "AEGIS_SQL_MODE",
	"AEGIS_RULES_RELOAD", "AEGIS_MINER_THRESHOLD", "AEGIS_MINER_PROMOTE_AFTER",
	"AEGIS_MINER_INTERVAL", "AEGIS_TELEGRAM_TOKEN", "AEGIS_TELEGRAM_CHATS",
	"AEGIS_TELEGRAM_MODE", "AEGIS_TELEGRAM_WEBHOOK_URL",
	"AEGIS_TELEGRAM_WEBHOOK_SECRET", "AEGIS_TELEGRAM_WORKERS",
	"AEGIS_TELEGRAM_API_BASE", "AEGIS_DOWNLOAD_TIMEOUT", "AEGIS_DOWNLOAD_MAX_BYTES",
	"AEGIS_DOWNLOAD_ALLOW_PRIVATE", "AEGIS_CLASSIFIER",
}

// baseEnv blanks everything Load reads, then applies the offline Build
// baseline: LLM kill switch on, temp store, temp workspace, background
// loops (rules reload, periodic miner) disabled. mutate runs last so each
// test can override any knob — including the baseline itself.
func baseEnv(t *testing.T, mutate func()) {
	t.Helper()
	for _, k := range appEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv("AEGIS_LLM", "off")
	t.Setenv("AEGIS_DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	t.Setenv("AEGIS_WORKSPACE", t.TempDir())
	t.Setenv("AEGIS_RULES_RELOAD", "0")
	t.Setenv("AEGIS_MINER_INTERVAL", "0")
	if mutate != nil {
		mutate()
	}
}

// quietLogger keeps Build's boot logs out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildWith boots the app under the baseline env. The returned cleanup is
// nil exactly when Build failed — Build tears down its own partial state on
// the error path, so failing callers must not invoke it.
func buildWith(t *testing.T, mutate func()) (*app.App, func(), error) {
	t.Helper()
	baseEnv(t, mutate)
	return app.Build(context.Background(), config.Load(),
		config.TierSmart, "app-test", quietLogger())
}

// errContains fails the test unless err is non-nil and mentions every want
// substring — the error-path assertions all check the user-facing wrap.
func errContains(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Build error = nil, want one mentioning %v", want)
	}
	for _, sub := range want {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("Build error = %q, want it to contain %q", err, sub)
		}
	}
}

// eventually polls cond until it holds or the deadline expires; the
// Telegram paths are asynchronous (worker pool, poll loop), so tests wait
// for observable effects instead of sleeping fixed amounts.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeBotAPI is a minimal scripted Bot API endpoint (the telegram package's
// own helper is package-private): a loopback httptest server dispatching on
// the method name — the URL path suffix after /bot<token>/ — recording
// request bodies under a mutex and replying from per-method response queues
// with a valid {"ok":true,...} envelope as fallback. Real client code runs
// against it through the AEGIS_TELEGRAM_API_BASE seam.
type fakeBotAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []botCall
	queue map[string][]fakeResponse
}

type botCall struct {
	method string
	path   string
	body   map[string]any
}

type fakeResponse struct {
	status int
	body   string
}

// newFakeBotAPI starts the server; it shuts down with the test.
func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{t: t, queue: make(map[string][]fakeResponse)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// defaultEnvelope answers each supported method when the test scripted
// nothing — getMe ok, empty getUpdates, generic true for the rest.
func defaultEnvelope(method string) string {
	switch method {
	case "getMe":
		return `{"ok":true,"result":{"id":1,"username":"fake_bot"}}`
	case "sendMessage":
		return `{"ok":true,"result":{"message_id":4242}}`
	case "getUpdates":
		return `{"ok":true,"result":[]}`
	default: // setWebhook, deleteWebhook, editMessageText
		return `{"ok":true,"result":true}`
	}
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := path[strings.LastIndex(path, "/")+1:]

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The client only ever posts JSON; anything else is a client bug
		// the test must hear about.
		f.t.Errorf("fakeBotAPI: %s request body is not JSON: %v", method, err)
	}

	f.mu.Lock()
	f.calls = append(f.calls, botCall{method: method, path: path, body: body})
	var resp fakeResponse
	if q := f.queue[method]; len(q) > 0 {
		resp, f.queue[method] = q[0], q[1:]
	}
	f.mu.Unlock()

	if resp.body == "" {
		resp.body = defaultEnvelope(method)
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

// respond queues the next reply for method; status 0 means 200.
func (f *fakeBotAPI) respond(method string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[method] = append(f.queue[method], fakeResponse{status: status, body: body})
}

// bodies returns the JSON bodies sent to method, in order.
func (f *fakeBotAPI) bodies(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.body)
		}
	}
	return out
}

func TestBuildRouterOnlyMode(t *testing.T) {
	a, cleanup, err := buildWith(t, func() {
		// Promote-after is wired from config into the engine package
		// global; pin that Build actually forwards the knob.
		t.Setenv("AEGIS_MINER_PROMOTE_AFTER", "2")
	})
	if err != nil {
		t.Fatalf("Build (router-only): %v", err)
	}
	if a.Engine == nil || a.Store == nil {
		t.Fatalf("Build returned nil engine or store: %+v", a)
	}
	if a.Engine.LLM != nil {
		t.Error("AEGIS_LLM=off must leave the engine without a provider")
	}
	if a.Webhook != nil {
		t.Error("no telegram token must leave Webhook nil")
	}
	if engine.PromoteAfter != 2 {
		t.Errorf("engine.PromoteAfter = %d, want 2 from AEGIS_MINER_PROMOTE_AFTER", engine.PromoteAfter)
	}

	// Real end-to-end pipeline: seeded rule → system_command → audit row.
	id, ctx := trace.New(context.Background(), "router-only-trace")
	res := a.Engine.Run(ctx, "/hostname")
	if res.DecisionSource != "regex_router" {
		t.Errorf("DecisionSource = %q, want regex_router", res.DecisionSource)
	}
	if res.RuleID != "hostname" {
		t.Errorf("RuleID = %q, want hostname", res.RuleID)
	}
	if res.TraceID != id {
		t.Errorf("TraceID = %q, want %q", res.TraceID, id)
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Error("router answer is empty, want the hostname output")
	}
	cleanup()
}

// TestBuildNilLoggerUsesDefault covers Build's nil-logger fallback (commands
// may pass no logger; Build must substitute slog.Default, not panic).
func TestBuildNilLoggerUsesDefault(t *testing.T) {
	baseEnv(t, nil)
	_, cleanup, err := app.Build(context.Background(), config.Load(),
		config.TierSmart, "app-test", nil)
	if err != nil {
		t.Fatalf("Build with nil logger: %v", err)
	}
	cleanup()
}

func TestBuildBadMCPSpec(t *testing.T) {
	_, cleanup, err := buildWith(t, func() {
		t.Setenv("AEGIS_MCP_SERVERS", "bogus")
	})
	errContains(t, err, `mcp server "bogus"`, "unsupported spec")
	if cleanup != nil {
		t.Error("cleanup must be nil when Build fails")
	}
}

func TestBuildMCPSpecNoCommand(t *testing.T) {
	_, _, err := buildWith(t, func() {
		t.Setenv("AEGIS_MCP_SERVERS", "stdio:")
	})
	errContains(t, err, `mcp server "stdio:"`, "stdio spec needs a command")
}

func TestBuildLLMOnInvalidProvider(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		_, _, err := buildWith(t, func() {
			t.Setenv("AEGIS_LLM", "on")
			t.Setenv("AEGIS_PROVIDER", "nope")
			t.Setenv("AEGIS_MODEL", "m1")
		})
		errContains(t, err, `unknown AEGIS_PROVIDER "nope"`)
	})
	t.Run("missing provider", func(t *testing.T) {
		_, _, err := buildWith(t, func() {
			t.Setenv("AEGIS_LLM", "on")
		})
		errContains(t, err, "AEGIS_PROVIDER is required")
	})
}

// TestBuildLLMOnConstructsProviderOffline: with the fallback on, Build
// validates config and constructs the provider object — purely in-memory,
// no request is made. The engine is then exercised with a ROUTER hit only
// (seed rules never sample), so the provider is never invoked and the test
// stays offline.
func TestBuildLLMOnConstructsProviderOffline(t *testing.T) {
	a, cleanup, err := buildWith(t, func() {
		t.Setenv("AEGIS_LLM", "on")
		t.Setenv("AEGIS_PROVIDER", "openai")
		t.Setenv("OPENAI_API_KEY", "test-key")
		t.Setenv("AEGIS_MODEL", "m1")
	})
	if err != nil {
		t.Fatalf("Build with LLM on: %v", err)
	}
	if a.Engine.LLM == nil {
		t.Fatal("LLM on must wire a provider into the engine")
	}
	if a.Engine.Model != "m1" {
		t.Errorf("engine model = %q, want m1", a.Engine.Model)
	}
	_, ctx := trace.New(context.Background(), "llm-on-trace")
	res := a.Engine.Run(ctx, "/uptime")
	if res.DecisionSource != "regex_router" {
		t.Errorf("DecisionSource = %q, want regex_router (router hit, provider untouched)", res.DecisionSource)
	}
	cleanup()
}

// TestBuildStoreOpenFails covers Build's first error arm: SQLite cannot open
// a directory as a database file, so a bogus AEGIS_DB_PATH must fail loudly.
func TestBuildStoreOpenFails(t *testing.T) {
	_, _, err := buildWith(t, func() {
		t.Setenv("AEGIS_DB_PATH", t.TempDir()) // a directory, not a file
	})
	errContains(t, err, "opening store")
}

// TestBuildBadStoredRulePattern: Build reads rules from the store, not just
// the code seed set — a row with an uncompilable pattern must abort startup
// instead of silently routing around it. Pre-seeding the temp DB also
// exercises LoadRules' read path (rules exist, no reseeding).
func TestBuildBadStoredRulePattern(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rules.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	ctx := context.Background()
	if err := st.Exec(ctx,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts)
		 VALUES ('broken', '((', 'system_command', '{}', 'seed', 1, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("inserting bad rule: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing seeding store: %v", err)
	}

	_, _, err = buildWith(t, func() {
		t.Setenv("AEGIS_DB_PATH", dbPath)
	})
	errContains(t, err, "rule broken")
}

func TestTelegramBadTokenFailsBuild(t *testing.T) {
	fake := newFakeBotAPI(t)
	fake.respond("getMe", http.StatusUnauthorized, `{"ok":false,"description":"Unauthorized"}`)

	_, _, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "rejected-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_CHATS", "424242")
	})
	errContains(t, err, "token rejected (getMe)", "HTTP 401")
}

func TestTelegramPollMode(t *testing.T) {
	fake := newFakeBotAPI(t) // getMe ok, getUpdates → empty result

	a, cleanup, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "poll-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_MODE", "poll")
		t.Setenv("AEGIS_TELEGRAM_CHATS", "424242")
	})
	if err != nil {
		t.Fatalf("Build (poll mode): %v", err)
	}
	if a.Webhook != nil {
		t.Error("poll mode must leave Webhook nil")
	}
	// The long-poll loop must actually run against the transport seam.
	eventually(t, "the long-poll loop to fetch updates",
		func() bool { return len(fake.bodies("getUpdates")) > 0 })
	cleanup()
}

func TestTelegramWebhookMode(t *testing.T) {
	fake := newFakeBotAPI(t)

	a, cleanup, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "hook-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_MODE", "webhook")
		t.Setenv("AEGIS_TELEGRAM_WEBHOOK_URL", "https://bot.example.com/hook")
		t.Setenv("AEGIS_TELEGRAM_CHATS", "424242")
	})
	if err != nil {
		t.Fatalf("Build (webhook mode): %v", err)
	}
	if a.Webhook == nil {
		t.Fatal("webhook mode must mount an HTTP handler")
	}
	hooks := fake.bodies("setWebhook")
	if len(hooks) != 1 {
		t.Fatalf("setWebhook calls = %d, want 1", len(hooks))
	}
	if want := "https://bot.example.com/hook" + server.WebhookPath; hooks[0]["url"] != want {
		t.Errorf("setWebhook url = %v, want %q", hooks[0]["url"], want)
	}
	// Secret unset in env: Build mints one per boot rather than running the
	// webhook without authentication.
	if secret, _ := hooks[0]["secret_token"].(string); secret == "" {
		t.Error("setWebhook secret_token is empty, want a per-boot minted secret")
	}
	cleanup()
}

func TestTelegramWebhookMissingURL(t *testing.T) {
	fake := newFakeBotAPI(t)

	_, _, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "hook-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_MODE", "webhook")
		// No AEGIS_TELEGRAM_WEBHOOK_URL, and no chats either — this boot
		// also covers the empty-allowlist warning path.
	})
	errContains(t, err, "needs AEGIS_TELEGRAM_WEBHOOK_URL")
}

func TestTelegramSetWebhookFails(t *testing.T) {
	fake := newFakeBotAPI(t)
	fake.respond("setWebhook", http.StatusInternalServerError, `{"ok":false,"description":"Internal"}`)

	_, _, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "hook-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_MODE", "webhook")
		t.Setenv("AEGIS_TELEGRAM_WEBHOOK_URL", "https://bot.example.com/hook")
		t.Setenv("AEGIS_TELEGRAM_CHATS", "424242")
	})
	errContains(t, err, "setWebhook failed", "HTTP 500")
}

// TestTelegramWebhookEndToEnd drives the full Telegram path through the
// mounted handler: signed delivery → durable inbox → worker pool →
// dispatcher → engine (router-only) → reply recorded at the Bot API. A
// wrongly signed delivery is rejected before the inbox. "/rules" exercises
// the rules-listing closure Build hands the dispatcher (live router defs).
func TestTelegramWebhookEndToEnd(t *testing.T) {
	fake := newFakeBotAPI(t)

	a, cleanup, err := buildWith(t, func() {
		t.Setenv("AEGIS_TELEGRAM_TOKEN", "hook-token")
		t.Setenv("AEGIS_TELEGRAM_API_BASE", fake.srv.URL)
		t.Setenv("AEGIS_TELEGRAM_MODE", "webhook")
		t.Setenv("AEGIS_TELEGRAM_WEBHOOK_URL", "https://bot.example.com/hook")
		t.Setenv("AEGIS_TELEGRAM_WEBHOOK_SECRET", "s3cr3t")
		t.Setenv("AEGIS_TELEGRAM_CHATS", "424242")
	})
	if err != nil {
		t.Fatalf("Build (webhook e2e): %v", err)
	}
	if hooks := fake.bodies("setWebhook"); len(hooks) != 1 || hooks[0]["secret_token"] != "s3cr3t" {
		t.Fatalf("setWebhook bodies = %v, want the configured secret", hooks)
	}

	postUpdate := func(payload, secret string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, server.WebhookPath, strings.NewReader(payload))
		req.Header.Set(telegram.WebhookHeader, secret)
		rec := httptest.NewRecorder()
		a.Webhook.ServeHTTP(rec, req)
		return rec.Code
	}
	update := func(id int64, text string) string {
		return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":1,`+
			`"chat":{"id":424242,"type":"private"},"from":{"id":8,"username":"nick"},"text":%q}}`, id, text)
	}

	if code := postUpdate(update(9000, "/uptime"), "wrong-secret"); code != http.StatusUnauthorized {
		t.Errorf("unsigned delivery status = %d, want 401", code)
	}
	for _, u := range []struct {
		id   int64
		text string
	}{
		{9001, "/uptime"},
		{9002, "/rules"},
	} {
		if code := postUpdate(update(u.id, u.text), "s3cr3t"); code != http.StatusOK {
			t.Errorf("delivery of %s status = %d, want 200", u.text, code)
		}
	}

	// Workers process asynchronously; wait until both replies landed.
	eventually(t, "two replies at the Bot API",
		func() bool { return len(fake.bodies("sendMessage")) >= 2 })
	var texts []string
	for _, b := range fake.bodies("sendMessage") {
		if txt, _ := b["text"].(string); txt != "" {
			texts = append(texts, txt)
		}
	}
	hasText := func(sub string) bool {
		for _, txt := range texts {
			if strings.Contains(txt, sub) {
				return true
			}
		}
		return false
	}
	// "/uptime" answers deterministically with a decision header naming the
	// rule and source — the telegram surface shows cost behavior.
	if !hasText("regex_router via uptime") {
		t.Errorf("replies %q, none carries the router decision header", texts)
	}
	// "/rules" renders the live router defs through Build's closure.
	if !hasText("Active rules:") || !hasText("uptime → /uptime → system_command") {
		t.Errorf("replies %q, none lists the active rules", texts)
	}
	// The rejected delivery must never reach the inbox, so exactly two
	// replies exist (claim-then-send keeps redeliveries from doubling).
	if n := len(fake.bodies("sendMessage")); n != 2 {
		t.Errorf("sendMessage calls = %d, want exactly 2", n)
	}
	cleanup()
}
