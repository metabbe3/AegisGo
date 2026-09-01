package provider

import (
	"log/slog"
	"strings"
	"testing"

	"aegisgo/internal/config"
)

// Every test in this file is construction-only: all four backend
// constructors (openaiprovider.NewChatCompletionsAgent,
// anthropicprovider.NewAgent, foundryprovider.NewAgent, and
// azidentity.NewDefaultAzureCredential) build their client, options, and
// middleware graph without network I/O. RunText/Run/RunMessage are the only
// methods that dial a provider, and no test here calls them.

// bothTiers drives each backend through both Tier values so every model-tier
// resolution path into New is exercised at least once.
var bothTiers = []config.Tier{config.TierFast, config.TierSmart}

// construct runs New for one tier and asserts the invariants shared by every
// offline-construction case: nil error, a non-nil agent, and the identity
// fields New sets in the shared agent.Config.
func construct(t *testing.T, cfg config.Config, tier config.Tier, wantProvider string) {
	t.Helper()
	a, err := New(cfg, tier, nil, slog.Default())
	if err != nil {
		t.Fatalf("New(provider=%s, tier=%s) unexpected error: %v", cfg.Provider, tier, err)
	}
	if a == nil {
		t.Fatalf("New(provider=%s, tier=%s) returned nil agent with nil error", cfg.Provider, tier)
	}
	if got := a.ProviderName(); got != wantProvider {
		t.Errorf("ProviderName() = %q, want %q", got, wantProvider)
	}
	if got := a.Name(); got != "aegis" {
		t.Errorf("Name() = %q, want %q (set by New for every backend)", got, "aegis")
	}
}

func TestNewNoModel(t *testing.T) {
	// ModelFor falls back across tiers, so the error only fires when every
	// model field is empty. The tier value is part of the message, so both
	// Tier constants are pinned through New's error path.
	for _, tier := range bothTiers {
		cfg := config.Config{Provider: config.ProviderOpenAI}
		a, err := New(cfg, tier, nil, slog.Default())
		if err == nil || a != nil {
			t.Fatalf("New(tier=%s) with no model fields = agent %v, err %v; want nil agent and an error", tier, a, err)
		}
		want := `no model configured for tier "` + string(tier) + `"`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New(tier=%s) error = %q, want it to contain %q", tier, err.Error(), want)
		}
	}
}

func TestNewUnsupportedProvider(t *testing.T) {
	// A model must be set, otherwise the model check rejects first and the
	// provider switch is never reached.
	cfg := config.Config{Provider: "bogus", Model: "some-model"}
	a, err := New(cfg, config.TierSmart, nil, slog.Default())
	if err == nil || a != nil {
		t.Fatalf("New(bogus) = agent %v, err %v; want nil agent and an error", a, err)
	}
	if !strings.Contains(err.Error(), `unsupported provider "bogus"`) {
		t.Errorf("New(bogus) error = %q, want it to contain %q", err.Error(), `unsupported provider "bogus"`)
	}
}

func TestNewOpenAI(t *testing.T) {
	// Only the tier-specific model fields are set (no Model), so a
	// successful construction proves the tier models flow through
	// ModelFor into New on both tiers.
	cfg := config.Config{
		Provider:   config.ProviderOpenAI,
		ModelFast:  "test-model-fast",
		ModelSmart: "test-model-smart",
		OpenAIKey:  "test-key",
	}
	for _, tier := range bothTiers {
		construct(t, cfg, tier, "openai")
	}
}

func TestNewOpenAICompat(t *testing.T) {
	cfg := config.Config{
		Provider:      config.ProviderOpenAICompat,
		ModelFast:     "test-model-fast",
		ModelSmart:    "test-model-smart",
		OpenAIKey:     "test-key",
		OpenAIBaseURL: "http://localhost:11434/v1",
	}
	for _, tier := range bothTiers {
		construct(t, cfg, tier, "openai-compatible")
	}
}

func TestNewAnthropic(t *testing.T) {
	// A custom Instructions value exercises the override branch of New's
	// instructions ternary. The instructions themselves are captured into
	// the agent's opaque run options, so only construction is asserted.
	cfg := config.Config{
		Provider:     config.ProviderAnthropic,
		ModelFast:    "test-model-fast",
		ModelSmart:   "test-model-smart",
		AnthropicKey: "test-key",
		Instructions: "custom persona for tests",
	}
	for _, tier := range bothTiers {
		construct(t, cfg, tier, "anthropic")
	}
}

func TestNewFoundry(t *testing.T) {
	cfg := config.Config{
		Provider:        config.ProviderFoundry,
		Model:           "test-deployment",
		FoundryEndpoint: "https://example.invalid/",
	}
	a, err := New(cfg, config.TierSmart, nil, slog.Default())
	// azidentity's default chain builds lazily and performs no network I/O
	// at construction, but on a machine with no configured credentials it
	// may refuse to build at all — New wraps that as "azure credential:
	// ...". Success and credential-refusal are both acceptable outcomes;
	// a nil agent paired with a nil error is not, and neither is an
	// unrelated error.
	if err != nil {
		if a != nil {
			t.Fatalf("New(foundry) returned agent %v and err %v; want nil agent on error", a, err)
		}
		if !strings.Contains(err.Error(), "credential") {
			t.Fatalf("New(foundry) error = %q, want it to mention credentials", err.Error())
		}
		return
	}
	if a == nil {
		t.Fatalf("New(foundry) returned nil agent with nil error")
	}
	if got := a.ProviderName(); got != "microsoft.foundry" {
		t.Errorf("ProviderName() = %q, want %q", got, "microsoft.foundry")
	}
	if got := a.Name(); got != "aegis" {
		t.Errorf("Name() = %q, want %q", got, "aegis")
	}
}
