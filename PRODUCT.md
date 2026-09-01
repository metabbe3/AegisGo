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
| Serving | REST: sync + `?async=1` (202 + poll), /healthz, /readyz; systemd unit |
| Durable substrate | embedded SQLite: audit trail, rules, fallback corpus, answers; single-writer batcher |

## Scope Boundaries

**In**: single-agent services, file/SQL/system tools, MCP client, REST
serving, multi-provider config, hybrid routing with audit.

**Out (for now)**: gRPC, Telegram, streaming, multi-agent orchestration,
conversation persistence, REST authn — see roadmap.

## Cost Strategy

Three stacked mechanisms:

1. **Route around the model** — every rule hit is $0 and ~0ms.
2. **Tier the fallback** — fast model for routine, smart model when needed.
3. **Mine the misses** (Phase 3) — repeated fallback shapes become rules, so
   LLM spend falls as usage grows.

## Roadmap

### Phase 2 — more interfaces
- **gRPC + protobuf contract** (`.proto`, protoc codegen in Makefile) so
  PHP/Python/Java services call the same engine; async semantics map to the
  existing answer store (Run + AnswerPoll / server-stream).
- **Telegram dual-mode**: one dispatcher, two transports — webhook (public
  TLS ingress) and getUpdates long-poll (NAT'd boxes, zero ingress). Design
  constraints locked in from the ideation pass: ack-then-process (Telegram
  retries slow webhooks → duplicate side effects), dedupe on monotonically
  increasing update_id with a persisted high-water mark (replay protection),
  at-least-once delivery + idempotent replies, chat allowlist, per-chat rate
  limits, placeholder-then-editMessageText UX. First build step is the
  crash-window test (high-water vs process vs send ordering).

### Phase 3 — the self-optimizing router
- **Rule miner**: cluster `fallback_events` by normalized prompt shape;
  candidates enter **shadow mode** (rule runs, LLM still answers, comparator
  logs agree/diverge); auto-promote after N agreements; **1% permanent
  shadow sampling with auto-demotion** — the load-bearing guard, because a
  wrong promoted rule answers wrongly forever while the cost dashboard
  improves. Promotion ladder: candidate → shadow → canary → full.
- **ROI-ranked proposals** (frequency × cost saved), parameterized rules
  with capture groups, `aegisgo rules list|promote|disable|export` admin
  surface, rules-as-YAML for PR review.
- **SSE streaming** on /v1/agent/run (deterministic answers sliced through
  the same contract), `/stats` endpoint (regex deflection rate — the
  headline metric — plus cost/latency per provider/interface), `aegisgo
  replay --trace` regression harness over the audit table.

## Non-Goals

- Being a framework. AegisGo is a template to fork.
- Supporting every agent SDK — one loop (agent-framework-go), one MCP
  library (mcp-go), chosen deliberately and documented in CLAUDE.md.
- Inline shell execution, ever. The system_command catalog is fixed-argv.
