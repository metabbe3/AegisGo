package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModelFor(t *testing.T) {
	c := Config{Model: "base", ModelFast: "cheap"}
	if got := c.ModelFor(TierSmart); got != "base" {
		t.Errorf("smart = %q, want base", got)
	}
	if got := c.ModelFor(TierFast); got != "cheap" {
		t.Errorf("fast = %q, want cheap", got)
	}

	c = Config{ModelFast: "cheap", ModelSmart: "strong"}
	if got := c.ModelFor(TierFast); got != "cheap" {
		t.Errorf("fast = %q, want cheap", got)
	}
	if got := c.ModelFor(TierSmart); got != "strong" {
		t.Errorf("smart = %q, want strong", got)
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("empty config should not validate")
	}
	ok := Config{
		Provider:      ProviderOpenAICompat,
		Model:         "llama3",
		OpenAIKey:     "k",
		OpenAIBaseURL: "http://localhost:11434/v1",
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	missingURL := ok
	missingURL.OpenAIBaseURL = ""
	if err := missingURL.Validate(); err == nil {
		t.Error("openai-compat without base URL should not validate")
	}
	badProvider := ok
	badProvider.Provider = "nope"
	if err := badProvider.Validate(); err == nil {
		t.Error("unknown provider should not validate")
	}
}

func TestParseTier(t *testing.T) {
	for in, want := range map[string]Tier{"": TierSmart, "smart": TierSmart, "FAST": TierFast} {
		got, err := ParseTier(in)
		if err != nil || got != want {
			t.Errorf("ParseTier(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseTier("turbo"); err == nil {
		t.Error("unknown tier should error")
	}
}

// clearConfigEnv blanks every environment variable Load reads so the tests
// below are hermetic even when the developer's shell exports AEGIS_* values.
// Load treats a blank value exactly like an unset one (envOr / AtoiDefault /
// parseList all fall back on empty input), so "" is a faithful "unset".
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AEGIS_PROVIDER", "AEGIS_MODEL", "AEGIS_MODEL_FAST", "AEGIS_MODEL_SMART",
		"AEGIS_INSTRUCTIONS", "OPENAI_API_KEY", "OPENAI_BASE_URL",
		"ANTHROPIC_API_KEY", "FOUNDRY_ENDPOINT", "AEGIS_WORKSPACE",
		"AEGIS_MCP_SERVERS", "AEGIS_ADDR", "AEGIS_GRPC_ADDR", "AEGIS_LOG_LEVEL",
		"AEGIS_LLM", "AEGIS_DB_PATH", "AEGIS_SQL_DSN", "AEGIS_SQL_MODE",
		"AEGIS_RULES_RELOAD", "AEGIS_MINER_THRESHOLD", "AEGIS_MINER_PROMOTE_AFTER",
		"AEGIS_MINER_INTERVAL", "AEGIS_TELEGRAM_TOKEN", "AEGIS_TELEGRAM_CHATS",
		"AEGIS_TELEGRAM_MODE", "AEGIS_TELEGRAM_WEBHOOK_URL",
		"AEGIS_TELEGRAM_WEBHOOK_SECRET", "AEGIS_TELEGRAM_WORKERS",
		"AEGIS_TELEGRAM_API_BASE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)
	got := Load()
	want := Config{
		Addr:            ":8080",
		GRPCAddr:        ":8081",
		LogLevel:        "info",
		LLM:             "on",
		DBPath:          "aegisgo.db",
		SQLMode:         "ro",
		RulesReloadSecs: 30,

		MinerThreshold:    20,
		MinerPromoteAfter: 5,
		MinerIntervalSecs: 3600,

		TelegramMode:    "auto",
		TelegramWorkers: 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() with cleared env mismatch:\n got  %+v\n want %+v", got, want)
	}
	if got.LLMDisabled() {
		t.Error("LLM fallback must default to on")
	}
	if got.TelegramEnabled() {
		t.Error("Telegram must default to dormant (no token)")
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("AEGIS_PROVIDER", " openai ")
	t.Setenv("AEGIS_MODEL", " gpt-4o ")
	t.Setenv("AEGIS_MODEL_FAST", " gpt-4o-mini ")
	t.Setenv("AEGIS_MODEL_SMART", " gpt-4o ")
	t.Setenv("AEGIS_INSTRUCTIONS", " be terse ")
	t.Setenv("OPENAI_API_KEY", " sk-x ")
	t.Setenv("OPENAI_BASE_URL", " http://localhost:11434/v1 ")
	t.Setenv("ANTHROPIC_API_KEY", " ak ")
	t.Setenv("FOUNDRY_ENDPOINT", " https://res.services.ai.azure.com/projects/p ")
	t.Setenv("AEGIS_WORKSPACE", " ./data ")
	t.Setenv("AEGIS_MCP_SERVERS", " a , b ,,")
	t.Setenv("AEGIS_ADDR", " :9090 ")
	t.Setenv("AEGIS_GRPC_ADDR", " :9091 ")
	t.Setenv("AEGIS_LOG_LEVEL", " debug ")
	t.Setenv("AEGIS_LLM", " off ")
	t.Setenv("AEGIS_DB_PATH", " /tmp/aegis-test.db ")
	t.Setenv("AEGIS_SQL_DSN", " file:aegis-test?mode=memory ")
	t.Setenv("AEGIS_SQL_MODE", " rw ")
	t.Setenv("AEGIS_RULES_RELOAD", "abc") // invalid int → default
	t.Setenv("AEGIS_MINER_THRESHOLD", " 25 ")
	t.Setenv("AEGIS_MINER_PROMOTE_AFTER", " 6 ")
	t.Setenv("AEGIS_MINER_INTERVAL", " 7200 ")
	t.Setenv("AEGIS_TELEGRAM_TOKEN", " tok ")
	// Non-numeric chat IDs are skipped, not fatal: the allowlist is
	// best-effort parsed so one bad entry cannot take the bot down.
	t.Setenv("AEGIS_TELEGRAM_CHATS", "42, -7, x,")
	t.Setenv("AEGIS_TELEGRAM_MODE", " webhook ")
	t.Setenv("AEGIS_TELEGRAM_WEBHOOK_URL", " https://bot.example.com/hook ")
	t.Setenv("AEGIS_TELEGRAM_WEBHOOK_SECRET", " s3cr3t ")
	t.Setenv("AEGIS_TELEGRAM_WORKERS", " 9 ")
	t.Setenv("AEGIS_TELEGRAM_API_BASE", " http://127.0.0.1:8081 ")

	got := Load()
	want := Config{
		Provider:        "openai",
		Model:           "gpt-4o",
		ModelFast:       "gpt-4o-mini",
		ModelSmart:      "gpt-4o",
		Instructions:    "be terse",
		OpenAIKey:       "sk-x",
		OpenAIBaseURL:   "http://localhost:11434/v1",
		AnthropicKey:    "ak",
		FoundryEndpoint: "https://res.services.ai.azure.com/projects/p",
		Workspace:       "./data",
		MCPServers:      []string{"a", "b"},
		Addr:            ":9090",
		GRPCAddr:        ":9091",
		LogLevel:        "debug",
		LLM:             "off",
		DBPath:          "/tmp/aegis-test.db",
		SQLDSN:          "file:aegis-test?mode=memory",
		SQLMode:         "rw",
		RulesReloadSecs: 30,

		MinerThreshold:    25,
		MinerPromoteAfter: 6,
		MinerIntervalSecs: 7200,

		TelegramToken:         "tok",
		TelegramChats:         []int64{42, -7},
		TelegramMode:          "webhook",
		TelegramWebhookURL:    "https://bot.example.com/hook",
		TelegramWebhookSecret: "s3cr3t",
		TelegramWorkers:       9,
		TelegramAPIBase:       "http://127.0.0.1:8081",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() from env mismatch:\n got  %+v\n want %+v", got, want)
	}
	if !got.LLMDisabled() {
		t.Error("AEGIS_LLM=off must disable the LLM fallback")
	}
	if !got.TelegramEnabled() {
		t.Error("token set must enable Telegram")
	}
	if !got.TelegramUseWebhook() {
		t.Error("webhook mode must select the webhook transport")
	}
	if !got.GRPCEnabled() {
		t.Error("explicit gRPC addr must keep gRPC enabled")
	}
}

func TestTelegramEnabled(t *testing.T) {
	if (Config{}).TelegramEnabled() {
		t.Error("no token must keep Telegram dormant")
	}
	if !(Config{TelegramToken: "t"}).TelegramEnabled() {
		t.Error("token set must enable Telegram")
	}
}

func TestTelegramUseWebhookMatrix(t *testing.T) {
	url := "https://bot.example.com/hook"
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"webhook mode without URL", Config{TelegramMode: "webhook"}, true},
		{"poll mode with URL", Config{TelegramMode: "poll", TelegramWebhookURL: url}, false},
		{"auto with URL", Config{TelegramMode: "auto", TelegramWebhookURL: url}, true},
		{"auto without URL", Config{TelegramMode: "auto"}, false},
		// Any unrecognized mode lands in the auto branch.
		{"unknown mode with URL", Config{TelegramMode: "banana", TelegramWebhookURL: url}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TelegramUseWebhook(); got != tc.want {
				t.Errorf("TelegramUseWebhook() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLLMDisabledCaseFold(t *testing.T) {
	for _, llm := range []string{"off", "OFF", "Off", "oFf"} {
		if !(Config{LLM: llm}).LLMDisabled() {
			t.Errorf("LLM=%q must count as disabled", llm)
		}
	}
	for _, llm := range []string{"on", "", "off-now"} {
		if (Config{LLM: llm}).LLMDisabled() {
			t.Errorf("LLM=%q must not count as disabled", llm)
		}
	}
}

func TestGRPCEnabled(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8081", true},
		{":9090", true},
		{"none", false},
		{"NONE", false},
		{"None", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := (Config{GRPCAddr: tc.addr}).GRPCEnabled(); got != tc.want {
			t.Errorf("GRPCEnabled(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestWorkspaceDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if got := (Config{}).WorkspaceDir(); got != wd {
		t.Errorf("empty workspace = %q, want cwd %q", got, wd)
	}
	if got := (Config{Workspace: "testdata"}).WorkspaceDir(); got != filepath.Join(wd, "testdata") {
		t.Errorf("relative workspace = %q, want %q", got, filepath.Join(wd, "testdata"))
	}
	abs := filepath.Join(wd, "testdata")
	if got := (Config{Workspace: abs}).WorkspaceDir(); got != abs {
		t.Errorf("absolute workspace = %q, want %q", got, abs)
	}
	// WorkspaceDir's Getwd/Abs error fallbacks are unreachable from a test
	// without fault injection, so those two return statements stay uncovered.
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub string // substring the error must contain; empty means Validate must succeed
	}{
		{name: "missing provider", cfg: Config{Model: "m"}, wantSub: "AEGIS_PROVIDER is required"},
		{name: "unknown provider", cfg: Config{Provider: "nope", Model: "m"}, wantSub: `unknown AEGIS_PROVIDER "nope"`},
		{name: "missing model", cfg: Config{Provider: ProviderOpenAI, OpenAIKey: "k"}, wantSub: "AEGIS_MODEL is required"},
		{name: "openai without key", cfg: Config{Provider: ProviderOpenAI, Model: "gpt-4o"}, wantSub: "OPENAI_API_KEY"},
		{name: "openai-compat without key", cfg: Config{Provider: ProviderOpenAICompat, Model: "llama3", OpenAIBaseURL: "http://localhost:11434/v1"}, wantSub: "OPENAI_API_KEY"},
		{name: "openai-compat without base URL", cfg: Config{Provider: ProviderOpenAICompat, Model: "llama3", OpenAIKey: "k"}, wantSub: "OPENAI_BASE_URL"},
		{name: "anthropic without key", cfg: Config{Provider: ProviderAnthropic, Model: "claude-sonnet"}, wantSub: "ANTHROPIC_API_KEY"},
		{name: "foundry without endpoint", cfg: Config{Provider: ProviderFoundry, Model: "dep"}, wantSub: "FOUNDRY_ENDPOINT"},

		{name: "openai valid", cfg: Config{Provider: ProviderOpenAI, Model: "gpt-4o", OpenAIKey: "k"}},
		{name: "openai valid with fast-tier model only", cfg: Config{Provider: ProviderOpenAI, ModelFast: "gpt-4o-mini", OpenAIKey: "k"}},
		{name: "openai-compat valid", cfg: Config{Provider: ProviderOpenAICompat, Model: "llama3", OpenAIKey: "k", OpenAIBaseURL: "http://localhost:11434/v1"}},
		{name: "anthropic valid", cfg: Config{Provider: ProviderAnthropic, Model: "claude-sonnet", AnthropicKey: "k"}},
		{name: "foundry valid", cfg: Config{Provider: ProviderFoundry, Model: "dep", FoundryEndpoint: "https://res.services.ai.azure.com/projects/p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() error = %q, want it to name %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestModelForTierFallback(t *testing.T) {
	// Fast tier prefers ModelFast, then Model, then ModelSmart; smart tier
	// mirrors that with its own var first. Asserts the full firstNonEmpty
	// ordering, including the all-empty case.
	cases := []struct {
		name  string
		cfg   Config
		fast  string
		smart string
	}{
		{"tier vars win over Model", Config{Model: "m", ModelFast: "f", ModelSmart: "s"}, "f", "s"},
		{"fast falls back to Model", Config{Model: "m", ModelSmart: "s"}, "m", "s"},
		{"smart falls back to Model", Config{Model: "m", ModelFast: "f"}, "f", "m"},
		{"fast falls back to ModelSmart", Config{ModelSmart: "s"}, "s", "s"},
		{"smart falls back to ModelFast", Config{ModelFast: "f"}, "f", "f"},
		{"all empty", Config{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ModelFor(TierFast); got != tc.fast {
				t.Errorf("ModelFor(TierFast) = %q, want %q", got, tc.fast)
			}
			if got := tc.cfg.ModelFor(TierSmart); got != tc.smart {
				t.Errorf("ModelFor(TierSmart) = %q, want %q", got, tc.smart)
			}
		})
	}
}
