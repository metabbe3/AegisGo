# PRODUCT.md — What AegisGo Is

## Vision

AegisGo is a reusable Go template for **hybrid** AI agents on Linux servers:
deterministic Go code answers anything rule-shaped (instant, free, works
during provider outages); the LLM is strictly a fallback reasoner for
everything else. One static binary, no runtime dependencies, no container:
deploy with systemd, point it at any LLM provider via environment variables.

It exists to be copied. New agent service → clone this repo → rename → add
your tools and router rules. The plumbing (hybrid engine, audit trail,
rule hot-reload, SQL/system tools, MCP, multi-interface serving, path
safety) is already done.

## Users

- **Backend/platform engineers** adding an LLM-powered data assistant to a
  microservice fleet without adopting a heavy framework.
- **Teams optimizing LLM spend** — deterministic routing means most traffic
  never reaches a model; when it does, provider/model/tier are runtime
  choices (dev on free local Ollama, prod picks per-environment).
- **Operators on call** — `AEGIS_LLM=off` degrades to a router-only agent
  instead of an outage; liveness/readiness probes split so a provider outage
  never looks like a dead process.
- **AI agents themselves** (Claude Code et al.) — CLAUDE.md is the contract
  for safe automated changes to this codebase.

## The Hybrid Decision Pipeline (v1, shipped)

1. **Intercept & trace:** every request (REST today, CLI) gets a trace ID,
   echoed in responses and joined across slog lines and audit rows.
2. **Deterministic router (no AI):** anchored regex rules
   (`/uptime`, `/disk`, `/memory`, `/hostname`, `/kernel`, `/who`,
   `/csv_summary <path>`, `/csv_head <path> [n]`) execute registered Go
   tools natively and answer in milliseconds. Rules live in the SQLite
   rules table, hot-reload on an interval, and are editable at runtime.
3. **LLM fallback:** unmatched prompts go to the provider with all tools
   (builtins + MCP-bridged). Every fallback is persisted as normalized
   mining corpus for Phase 3.
4. **Audit:** one slog line + one durable row per run: trace_id, interface,
   decision_source (regex_router | llm | llm_disabled | error), rule_id,
   prompt hash, model, latency, outcome.

| Capability | How |
|---|---|
| Deterministic routing | internal/router (anchored rules, hot reload, capture→args splicing) |
| Native tools | read_csv, csv_stats, read_doc, system_command (fixed-argv catalog), sql_query (ro, CSV attach) |
| SQL | database/sql over embedded SQLite (modernc, pure Go); `AEGIS_SQL_DSN` for external DBs |
| LLM providers | OpenAI, OpenAI-compatible (Ollama/OpenRouter/vLLM), Anthropic, Foundry — env-switched |
| Cost tiers | `AEGIS_MODEL_FAST` / `AEGIS_MODEL_SMART` + `--tier` |
| Kill switch | `AEGIS_LLM=off` — router-only mode, zero credentials |
| MCP | agent consumes external MCP servers (stdio + streamable HTTP) via mark3labs/mcp-go |
| Serving | REST (sync + async-poll + **SSE streaming** + /stats), **gRPC** (Run/RunAsync/GetAnswer/Ready + health + reflection); systemd unit |
| **Telegram** | dual-transport (webhook or long-poll, one dispatcher), dormant until `AEGIS_TELEGRAM_TOKEN`; at-least-once + idempotent replies on update_id; placeholder-then-edit UX; chat allowlist + per-chat rate limit |
| **Self-mining rules** | fallback corpus → shadow rules (tool-choice comparison) → auto-promotion / auto-demotion; 1% permanent sampling on promoted rules |
| Admin | `aegisctl` — rules list/mine/promote/demote, stats, replay |
| Durable substrate | embedded SQLite: audit trail, rules + lifecycle, fallback corpus, shadow events, answers, telegram inbox; single-writer batcher |

## Scope Boundaries

**In**: single-agent services, file/SQL/system tools, MCP client, REST
serving, multi-provider config, hybrid routing with audit.

**Out (for now)**: authentication, multi-agent orchestration, conversation
persistence — see roadmap.

## Cost Strategy

Three stacked mechanisms, all shipped:

1. **Route around the model** — every rule hit is $0 and ~0ms.
2. **Tier the fallback** — fast model for routine, smart model when needed.
3. **Mine the misses** — repeated fallback shapes become shadow rules and
   promote on tool-choice agreement, so LLM spend falls as traffic grows.
   Guard rails: any divergence demotes instantly; promoted rules keep a
   permanent 1% sampled double-check. Falling spend can never come from
   wrong answers.

## Roadmap (future)

- REST/gRPC authentication (per-caller keys, rate budgets).
- gRPC server-streaming Run (SSE-equivalent over the wire).
- Conversation persistence across processes (sessions in the store).
- Multi-agent orchestration; ROI-ranked rule proposals (`frequency ×
  cost saved`) and rules-as-YAML export for PR review.
- `/stats` history rollups (deflection rate over time, not just snapshot).

## Non-Goals

- Being a framework. AegisGo is a template to fork.
- Supporting every agent SDK — one loop (agent-framework-go), one MCP
  library (mcp-go), chosen deliberately and documented in CLAUDE.md.
- Inline shell execution, ever. The system_command catalog is fixed-argv.
