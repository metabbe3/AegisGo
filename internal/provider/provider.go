// Package provider builds agent-framework-go agents from environment
// configuration. One binary, any backend: OpenAI, any OpenAI-compatible
// endpoint (Ollama, OpenRouter, vLLM, ...), Anthropic, or Microsoft Foundry.
// Callers also pick a model tier (fast/smart) per run — the cost knob for
// production.
package provider

import (
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/anthropics/anthropic-sdk-go"
	anthropt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/anthropicprovider"
	"github.com/microsoft/agent-framework-go/provider/foundryprovider"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/microsoft/agent-framework-go/tool"
	openaigo "github.com/openai/openai-go/v3"
	openaiopt "github.com/openai/openai-go/v3/option"

	"aegisgo/internal/config"
)

// defaultInstructions is the agent persona when AEGIS_INSTRUCTIONS is unset.
const defaultInstructions = `You are AegisGo, a focused data assistant running on a server.
You answer questions about files in the workspace using your tools.
Call read_csv, csv_stats, or read_doc to inspect files before answering.
If a question is outside the workspace data, say so plainly instead of guessing.`

// New builds a configured agent. tools are attached verbatim (builtins plus
// any MCP-bridged tools); tier selects which model identifier is used.
func New(cfg config.Config, tier config.Tier, tools []tool.Tool, logger *slog.Logger) (*agent.Agent, error) {
	model := cfg.ModelFor(tier)
	if model == "" {
		return nil, fmt.Errorf("no model configured for tier %q", tier)
	}
	instructions := cfg.Instructions
	if instructions == "" {
		instructions = defaultInstructions
	}
	base := agent.Config{
		Name:   "aegis",
		Logger: logger,
		Tools:  tools,
	}

	switch cfg.Provider {
	case config.ProviderOpenAI:
		oc := openaigo.NewClient(openaiopt.WithAPIKey(cfg.OpenAIKey))
		return openaiprovider.NewChatCompletionsAgent(oc, openaiprovider.AgentConfig{
			Model:        model,
			Instructions: instructions,
			Config:       base,
		}), nil

	case config.ProviderOpenAICompat:
		oc := openaigo.NewClient(
			openaiopt.WithAPIKey(cfg.OpenAIKey),
			openaiopt.WithBaseURL(cfg.OpenAIBaseURL),
		)
		return openaiprovider.NewChatCompletionsAgent(oc, openaiprovider.AgentConfig{
			Model:        model,
			Instructions: instructions,
			ProviderName: "openai-compatible",
			Config:       base,
		}), nil

	case config.ProviderAnthropic:
		ac := anthropic.NewClient(anthropt.WithAPIKey(cfg.AnthropicKey))
		return anthropicprovider.NewAgent(ac, anthropicprovider.AgentConfig{
			Model:        model,
			Instructions: instructions,
			Config:       base,
		}), nil

	case config.ProviderFoundry:
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("azure credential: %w", err)
		}
		return foundryprovider.NewAgent(cfg.FoundryEndpoint, cred,
			foundryprovider.ModelDeployment(model),
			foundryprovider.AgentConfig{
				Instructions: instructions,
				Config:       base,
			}), nil

	default:
		return nil, fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
}
