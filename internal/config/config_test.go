package config

import "testing"

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
		t.Errorf("fast = %q", got)
	}
	if got := c.ModelFor(TierSmart); got != "strong" {
		t.Errorf("smart = %q", got)
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
