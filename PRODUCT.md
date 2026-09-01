# PRODUCT.md — What AegisGo Is

## Vision

AegisGo is a reusable Go template for building AI agents that run as
lightweight services on Linux servers. One static binary, no runtime
dependencies, no container required: deploy with systemd, point it at any LLM
provider via environment variables, and let it read workspace files (CSV,
text docs) and call external tools over MCP.

It exists to be copied. New agent service → clone this repo → rename → add
your tools. The plumbing (agent loop, provider switching, MCP, serving,
config, path safety) is already done.

## Users

- **Backend/platform engineers** adding an LLM-powered data assistant to a
  microservice fleet without adopting a heavy framework.
- **Teams optimizing LLM spend** — the provider and model are runtime
  choices, so dev can run Ollama for free while production picks a cheap or
  capable model per environment.
- **AI agents themselves** (Claude Code et al.) — CLAUDE.md is the contract
  for safe automated changes to this codebase.

## Core Capabilities (v0)

| Capability | How |
|---|---|
| Agent loop, tool calling | microsoft/agent-framework-go |
| LLM providers | OpenAI, OpenAI-compatible (Ollama/OpenRouter/vLLM), Anthropic, Microsoft Foundry — env-switched |
| Cost tiers | `AEGIS_MODEL_FAST` / `AEGIS_MODEL_SMART` + `--tier` flag: cheap model for routine runs, strong model when needed |
| File tools | `read_csv`, `csv_stats`, `read_doc` — sandboxed to `AEGIS_WORKSPACE` |
| External tools | Consumes external MCP servers (stdio subprocess or streamable HTTP) via mark3labs/mcp-go |
| Serving | `aegis-serve`: REST (`POST /v1/agent/run`, `GET /healthz`), stdlib net/http, graceful shutdown |
| CLI | `aegis-agent`: one-shot or interactive REPL |

## Scope Boundaries

**In**: single-agent services, file/tools integrations, MCP client, REST
serving, multi-provider config, the MCP server example under `examples/`.

**Out (for now)**: vector stores/RAG, conversation persistence across
processes, multi-agent orchestration, streaming HTTP responses, auth on the
REST API, gRPC, Windows support. Roadmap items below may promote these.

## Cost Strategy

The reason every decision is env-driven: run the *same* binary in different
environments with different economics.

- Local/dev: `openai-compat` + Ollama (`OPENAI_BASE_URL=http://localhost:11434/v1`) — free.
- Batch/cheap work: `--tier fast` with a small model.
- Hard tasks: `--tier smart` with a frontier model.
- Per-environment model rotation without rebuilds; provider pricing changes
  get absorbed by editing env vars, not code.

## Roadmap

1. Streaming responses (SSE) on `/v1/agent/run`.
2. gRPC facade alongside REST for polyglot microservice fleets.
3. Optional authn/authz middleware for the serve binary.
4. Expose the agent itself as an MCP server (mcp-go server side; the
   `examples/mcp-echo-server` pattern, generalized).
5. Conversation sessions backed by pluggable storage.

## Non-Goals

- Being a framework. AegisGo is a template to fork, not a library to build
  an ecosystem on.
- Supporting every agent framework — one loop (agent-framework-go), one MCP
  SDK (mcp-go), chosen deliberately and documented in CLAUDE.md.
