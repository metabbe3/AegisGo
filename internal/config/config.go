// Package config loads all AegisGo runtime configuration from environment
// variables. Everything is env-driven so the same binary can run against
// OpenAI, any OpenAI-compatible endpoint (Ollama, OpenRouter, vLLM, ...),
// Anthropic, or Microsoft Foundry without a rebuild — and so production
// deployments can pick cheaper models per environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Provider names supported by the agent factory (internal/provider).
const (
	ProviderOpenAI       = "openai"        // official OpenAI API
	ProviderOpenAICompat = "openai-compat" // any OpenAI-compatible endpoint (Ollama, OpenRouter, vLLM, ...)
	ProviderAnthropic    = "anthropic"     // Anthropic Claude API
	ProviderFoundry      = "foundry"       // Microsoft Foundry / Azure OpenAI
)

// Tier selects a model class so callers can trade cost vs. quality per task.
type Tier string

const (
	TierFast  Tier = "fast"  // cheap model for simple/routed work
	TierSmart Tier = "smart" // capable model (default)
)

// Config is the full runtime configuration for AegisGo.
type Config struct {
	// Provider selects the LLM backend (see Provider* constants).
	Provider string
	// Model is the default model identifier (deployment name for Foundry).
	Model string
	// ModelFast overrides the model for TierFast runs; falls back to Model.
	ModelFast string
	// ModelSmart overrides the model for TierSmart runs; falls back to Model.
	ModelSmart string

	// Instructions is the agent system prompt.
	Instructions string

	// OpenAIKey is the OpenAI (or OpenAI-compatible) API key.
	OpenAIKey string
	// OpenAIBaseURL overrides the OpenAI API base URL (used by openai-compat).
	OpenAIBaseURL string

	// AnthropicKey is the Anthropic API key.
	AnthropicKey string

	// FoundryEndpoint is the Microsoft Foundry project endpoint
	// (https://<resource>.services.ai.azure.com/projects/<name>).
	FoundryEndpoint string

	// Workspace is the directory file tools may read from. Paths are
	// validated to stay inside it (symlink-aware). Defaults to the process
	// working directory.
	Workspace string

	// MCPServers lists external MCP servers to attach. Each entry is either
	// "stdio:<command> [args...]" or an http(s) URL of a streamable HTTP
	// MCP endpoint.
	MCPServers []string

	// Addr is the listen address for aegis-serve (default ":8080").
	Addr string

	// LogLevel is the slog level name: debug, info, warn, or error.
	LogLevel string

	// LLM toggles the fallback: "on" (default) or "off". Off makes every
	// router miss return fast without a provider — the kill switch.
	LLM string

	// DBPath is the embedded SQLite file for audit/rules/answers
	// (default "aegisgo.db" in the working directory).
	DBPath string

	// SQLDSN optionally points the sql_query tool at an external
	// database/sql DSN (empty = embedded SQLite at DBPath).
	SQLDSN string

	// SQLMode is "ro" (default) or "rw" for the sql_query tool.
	SQLMode string

	// RulesReloadSecs is the rules-table hot-reload interval; 0 disables.
	RulesReloadSecs int

	// TelegramToken enables the Telegram interface; empty (default) keeps
	// the whole subsystem dormant — built, wired, and one env var away.
	TelegramToken string
	// TelegramChats is the chat allowlist (numeric IDs). Empty denies all
	// processing (secure default); set it when enabling the bot.
	TelegramChats []int64
	// TelegramMode is "auto" (default: webhook when a URL is set, else
	// long-poll), "poll", or "webhook".
	TelegramMode string
	// TelegramWebhookURL is the public HTTPS webhook endpoint. Required in
	// webhook mode.
	TelegramWebhookURL string
	// TelegramWebhookSecret authenticates webhook deliveries; generated at
	// boot when empty.
	TelegramWebhookSecret string
	// TelegramWorkers bounds the dispatcher's worker pool (default 4).
	TelegramWorkers int
	// TelegramAPIBase overrides the Bot API base URL (tests, self-hosted
	// bot API servers); empty = api.telegram.org.
	TelegramAPIBase string
}

// Load reads configuration from the environment and applies defaults.
func Load() Config {
	return Config{
		Provider:        strings.TrimSpace(os.Getenv("AEGIS_PROVIDER")),
		Model:           strings.TrimSpace(os.Getenv("AEGIS_MODEL")),
		ModelFast:       strings.TrimSpace(os.Getenv("AEGIS_MODEL_FAST")),
		ModelSmart:      strings.TrimSpace(os.Getenv("AEGIS_MODEL_SMART")),
		Instructions:    strings.TrimSpace(os.Getenv("AEGIS_INSTRUCTIONS")),
		OpenAIKey:       strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:   strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		AnthropicKey:    strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		FoundryEndpoint: strings.TrimSpace(os.Getenv("FOUNDRY_ENDPOINT")),
		Workspace:       strings.TrimSpace(os.Getenv("AEGIS_WORKSPACE")),
		MCPServers:      parseList(os.Getenv("AEGIS_MCP_SERVERS")),
		Addr:            envOr("AEGIS_ADDR", ":8080"),
		LogLevel:        envOr("AEGIS_LOG_LEVEL", "info"),
		LLM:             envOr("AEGIS_LLM", "on"),
		DBPath:          envOr("AEGIS_DB_PATH", "aegisgo.db"),
		SQLDSN:          strings.TrimSpace(os.Getenv("AEGIS_SQL_DSN")),
		SQLMode:         envOr("AEGIS_SQL_MODE", "ro"),
		RulesReloadSecs: AtoiDefault(os.Getenv("AEGIS_RULES_RELOAD"), 30),

		TelegramToken:         strings.TrimSpace(os.Getenv("AEGIS_TELEGRAM_TOKEN")),
		TelegramChats:         parseInt64List(os.Getenv("AEGIS_TELEGRAM_CHATS")),
		TelegramMode:          envOr("AEGIS_TELEGRAM_MODE", "auto"),
		TelegramWebhookURL:    strings.TrimSpace(os.Getenv("AEGIS_TELEGRAM_WEBHOOK_URL")),
		TelegramWebhookSecret: strings.TrimSpace(os.Getenv("AEGIS_TELEGRAM_WEBHOOK_SECRET")),
		TelegramWorkers:       AtoiDefault(os.Getenv("AEGIS_TELEGRAM_WORKERS"), 4),
		TelegramAPIBase:       strings.TrimSpace(os.Getenv("AEGIS_TELEGRAM_API_BASE")),
	}
}

// TelegramEnabled reports whether the Telegram interface should boot.
func (c Config) TelegramEnabled() bool { return c.TelegramToken != "" }

// TelegramUseWebhook resolves the transport choice: explicit mode wins,
// auto picks webhook when a public URL is configured, else long-poll.
func (c Config) TelegramUseWebhook() bool {
	switch c.TelegramMode {
	case "webhook":
		return true
	case "poll":
		return false
	default: // auto
		return c.TelegramWebhookURL != ""
	}
}

// parseInt64List parses a comma-separated list of integers.
func parseInt64List(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		if v, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// LLMDisabled reports whether the LLM fallback kill switch is on.
func (c Config) LLMDisabled() bool { return strings.EqualFold(c.LLM, "off") }

// Validate reports configuration problems before anything dials out.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderOpenAI, ProviderOpenAICompat, ProviderAnthropic, ProviderFoundry:
	case "":
		return fmt.Errorf("AEGIS_PROVIDER is required: one of openai, openai-compat, anthropic, foundry")
	default:
		return fmt.Errorf("unknown AEGIS_PROVIDER %q (want openai, openai-compat, anthropic, or foundry)", c.Provider)
	}
	if c.Model == "" && c.ModelFast == "" && c.ModelSmart == "" {
		return fmt.Errorf("AEGIS_MODEL is required (or set AEGIS_MODEL_FAST / AEGIS_MODEL_SMART)")
	}
	switch c.Provider {
	case ProviderOpenAI, ProviderOpenAICompat:
		if c.OpenAIKey == "" {
			return fmt.Errorf("OPENAI_API_KEY is required for provider %s", c.Provider)
		}
		if c.Provider == ProviderOpenAICompat && c.OpenAIBaseURL == "" {
			return fmt.Errorf("OPENAI_BASE_URL is required for provider openai-compat (e.g. http://localhost:11434/v1 for Ollama)")
		}
	case ProviderAnthropic:
		if c.AnthropicKey == "" {
			return fmt.Errorf("ANTHROPIC_API_KEY is required for provider anthropic")
		}
	case ProviderFoundry:
		if c.FoundryEndpoint == "" {
			return fmt.Errorf("FOUNDRY_ENDPOINT is required for provider foundry")
		}
	}
	return nil
}

// ModelFor resolves the model to use for a tier. Explicit tier env vars win,
// then the general AEGIS_MODEL, then the other tier.
func (c Config) ModelFor(tier Tier) string {
	m := c.Model
	switch tier {
	case TierFast:
		m = firstNonEmpty(c.ModelFast, c.Model, c.ModelSmart)
	case TierSmart:
		m = firstNonEmpty(c.ModelSmart, c.Model, c.ModelFast)
	}
	return m
}

// WorkspaceDir returns the absolute workspace root, defaulting to the current
// working directory.
func (c Config) WorkspaceDir() string {
	if c.Workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	if abs, err := filepath.Abs(c.Workspace); err == nil {
		return abs
	}
	return c.Workspace
}

// ParseTier converts a CLI flag value into a Tier, defaulting to smart.
func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(TierSmart):
		return TierSmart, nil
	case string(TierFast):
		return TierFast, nil
	default:
		return TierSmart, fmt.Errorf("unknown tier %q (want fast or smart)", s)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// AtoiDefault parses s as an int, returning fallback when blank or invalid.
func AtoiDefault(s string, fallback int) int {
	if s = strings.TrimSpace(s); s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
