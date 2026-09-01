# AegisGo

Reusable Go template for a **hybrid** AI agent on a Linux server:
deterministic Go code answers anything rule-shaped — instantly, for free,
even during provider outages — and the LLM is strictly the fallback
reasoner for everything else. One static binary (pure Go, no CGo), audit
trail in embedded SQLite, MCP for external tools, REST for your
microservices.

Built on [microsoft/agent-framework-go] (agent loop), [mark3labs/mcp-go]
(MCP), and [modernc.org/sqlite] (pure-Go embedded storage). MIT licensed.

## The hybrid pipeline

```
request ──▶ trace ID ──▶ deterministic router ──match──▶ native Go tool ──▶ answer
 (REST/CLI)                  (regex rules,                       (csv/sql/system, ~0ms, $0)
                              hot-reloaded)                            │
                              │ miss                                  │
                              ▼                                       │
                          LLM fallback ─────── every run audited ─────┘
                          (provider+tier)      decision_source, trace_id,
                               │               rule_id, latency, cost corpus
                               ▼
                    AEGIS_LLM=off → fast "disabled" answer (kill switch)
```

## Quickstart

```bash
make build && make test      # no keys or network needed
AEGIS_LLM=off bin/aegis-agent /uptime        # instant, zero credentials
AEGIS_LLM=off bin/aegis-agent /csv_head testdata/sample.csv 5
AEGIS_LLM=off bin/aegis-agent "any question" # kill switch: no LLM configured
```

With a model behind it (free, local — [Ollama]):

```bash
export AEGIS_PROVIDER=openai-compat AEGIS_MODEL=qwen2.5:0.5b
export OPENAI_API_KEY=ollama OPENAI_BASE_URL=http://localhost:11434/v1
bin/aegis-agent "How many rows in testdata/sample.csv? Use the read_csv tool."
bin/aegis-agent --tier fast "quick question"   # cheap model tier
```

Or OpenAI / Anthropic / Foundry — see the env table.

## Self-mining rules (Phase 3)

Every LLM fallback is corpus. Repeated prompt shapes whose runs used one
derivable tool (`read_csv` / `csv_stats` / `read_doc`) become **shadow**
rules — they match but never answer; the engine runs the LLM anyway and
compares **tool choice**. Agreement streak (`AEGIS_MINER_PROMOTE_AFTER`,
default 5) promotes the rule to active; any divergence demotes it
instantly. Promoted rules keep a permanent 1% sampling check — divergence
auto-demotes. LLM spend falls as traffic grows, and the guard means falling
spend can never come from wrong answers.

```bash
aegisctl rules mine --threshold 3    # mine now (or let the hourly pass do it)
aegisctl rules list                  # see shadow/active/demoted
aegisctl stats                       # deflection rate, top fallback shapes
aegisctl replay <trace-id>           # audit trail for one run
```

Demo (live-verified): three LLM answers to "give me stats for file
testdata/sample.csv" → `rules mine` → two shadow agreements → promoted →
the same prompt answers in **0ms with `AEGIS_LLM=off`**.

## Serve it (REST + gRPC + SSE for your microservices)

```bash
AEGIS_LLM=off AEGIS_ADDR=:8080 bin/aegis-serve &
curl -s localhost:8080/healthz                       # liveness
curl -s localhost:8080/readyz                        # readiness (store reachable)
curl -s localhost:8080/v1/agent/run -d '{"prompt":"/uptime"}' -H 'X-Trace-Id: t-42'
#   {"output":"...","decision_source":"regex_router","trace_id":"t-42","latency_ms":3}
curl -s -X POST 'localhost:8080/v1/agent/run?async=1' -d '{"prompt":"hard question"}'
#   202 {"trace_id":"...","status":"pending"}   → poll GET /v1/answers/{trace_id}
curl -sN -X POST 'localhost:8080/v1/agent/run?stream=1' -d '{"prompt":"..."}'
#   text/event-stream: delta chunks + final done event (router hits AND LLM runs)
curl -s localhost:8080/v1/stats     # deflection rate, per-source latency, rule states
```

**gRPC** (default `:8081`, `AEGIS_GRPC_ADDR=none` disables) for PHP/Python/
Java callers — contract in `internal/pb/agent.proto`, stubs committed,
`make proto` regenerates:

```bash
grpcurl -plaintext -d '{"prompt":"/uptime"}' localhost:8081 aegisgo.v1.Agent/Run
# Run (sync) · RunAsync + GetAnswer (poll) · Ready · standard health + reflection
```

gRPC is on the roadmap (PRODUCT.md) riding the same engine.

## Telegram (built in, dormant until enabled)

The full Telegram interface ships in the binary — **one env var activates it**:

```bash
export AEGIS_TELEGRAM_TOKEN=123456:AB…       # from @BotFather
export AEGIS_TELEGRAM_CHATS=424242            # your numeric chat id (allowlist)
AEGIS_LLM=off bin/aegis-serve                 # even router-only bots work
```

Transport picks itself: `AEGIS_TELEGRAM_WEBHOOK_URL` set → webhook mode
(Telegram posts to `<url>/telegram/webhook`, secret-header validated);
unset → `getUpdates` long-polling (works behind NAT, zero ingress).
No token → the subsystem logs one line and stays out of the way.

Behavior: router commands (`/uptime`, `/csv_head …`) answer instantly and
free with a `[regex_router via rule · Nms]` header; anything else goes to
the LLM with a `…` placeholder edited into the answer. `/help` and `/rules`
work in-chat. Delivery is at-least-once with idempotent replies keyed on
`update_id` — webhook retries and crash replays never double-send. Unknown
chats are skipped, never answered. Ops:

```bash
sqlite3 aegisgo.db "SELECT status, COUNT(*) FROM telegram_inbox GROUP BY 1"
sqlite3 aegisgo.db "SELECT decision_source, COUNT(*) FROM audit_events WHERE interface='telegram' GROUP BY 1"
```

Other knobs: `AEGIS_TELEGRAM_MODE` (auto|poll|webhook),
`AEGIS_TELEGRAM_WEBHOOK_SECRET` (else generated per boot),
`AEGIS_TELEGRAM_WORKERS` (default 4), `AEGIS_TELEGRAM_API_BASE` (tests /
self-hosted Bot API).

## Router rules (deterministic, editable at runtime)

Shipped: `/uptime /disk /df /memory /free /hostname /kernel /uname /who
/csv_summary <path> /csv_head <path> [n]`. Rules live in the SQLite
`rules` table and hot-reload (`AEGIS_RULES_RELOAD`, default 30s):

```bash
sqlite3 aegisgo.db "INSERT INTO rules (name,pattern,tool,args_template,origin,enabled,created_ts)
  VALUES ('diskfree', '/diskfree', 'system_command', '{\"command\":\"disk\"}', 'seed', 1, datetime())"
```

Every LLM fallback is logged as normalized corpus — Phase 3 mines repeated
shapes into candidate rules (shadow → promote), so spend falls as traffic
grows. Watch it accumulating:

```bash
sqlite3 aegisgo.db "SELECT normalized_prompt, COUNT(*) FROM fallback_events GROUP BY 1 ORDER BY 2 DESC"
```

## Tools

| Tool | Notes |
|---|---|
| `read_csv` / `csv_stats` / `read_doc` | sandboxed to `AEGIS_WORKSPACE` (symlink-aware), size-capped |
| `system_command` | fixed-argv catalog (`uptime df free hostname uname who`) — input picks a key, never argv; process-group kill on timeout |
| `sql_query` | `database/sql`, parameterized only, **read-only by default** (`AEGIS_SQL_MODE=rw` to unlock); `attach_csvs` turns workspace CSVs into queryable temp tables |
| + external MCP tools | `AEGIS_MCP_SERVERS="stdio:<cmd> | https://host/mcp"` |

SQL over the sample CSV:

```bash
AEGIS_LLM=off bin/aegis-agent /csv_head testdata/sample.csv 1
# or via the LLM: "attach testdata/sample.csv as orders and total qty by shipped"
```

## Configuration (env)

| Variable | Meaning |
|---|---|
| `AEGIS_LLM` | `off` = router-only kill switch (no provider credentials needed) |
| `AEGIS_PROVIDER` | `openai` \| `openai-compat` \| `anthropic` \| `foundry` |
| `AEGIS_MODEL`, `AEGIS_MODEL_FAST/SMART` | default + per-tier models (`--tier fast/smart`) |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | OpenAI creds; base URL enables Ollama/OpenRouter/vLLM |
| `ANTHROPIC_API_KEY`, `FOUNDRY_ENDPOINT` | other providers |
| `AEGIS_WORKSPACE` | root file tools may read — security boundary |
| `AEGIS_MCP_SERVERS` | external MCP servers (stdio/HTTP) |
| `AEGIS_DB_PATH` | embedded SQLite file (audit, rules, answers) |
| `AEGIS_SQL_DSN` / `AEGIS_SQL_MODE` | sql_query backend / `ro`\|`rw` |
| `AEGIS_RULES_RELOAD` | rules hot-reload seconds (0 = off) |
| `AEGIS_ADDR` / `AEGIS_GRPC_ADDR` | HTTP / gRPC listen addresses (`none` disables gRPC) |
| `AEGIS_MINER_THRESHOLD` / `_PROMOTE_AFTER` / `_INTERVAL` | self-mining knobs (20 / 5 / 3600s, 0=off) |
| `AEGIS_TELEGRAM_TOKEN` | enables the Telegram interface (empty = dormant) |
| `AEGIS_TELEGRAM_CHATS` | chat allowlist (numeric IDs); empty denies all |
| `AEGIS_TELEGRAM_WEBHOOK_URL` / `_SECRET` / `_MODE` / `_WORKERS` / `_API_BASE` | transport tuning |

## Layout & Docs

- `CLAUDE.md` — rules for AI agents on this repo (hard rules: path sandbox,
  fixed-argv exec, SQL policy, single-writer store, decision_source contract)
- `PRODUCT.md` — vision, hybrid pipeline, cost strategy, phased roadmap
  (gRPC, Telegram dual-mode, self-mining router)
- `WORKFLOW.md` — recipes: add rules/tools/commands/providers, external SQL,
  operate the audit trail, Linux deploy
- `deploy/aegis-serve.service` — hardened systemd unit template
- `examples/mcp-echo-server` — the mcp-go server-side pattern

## Linux Deployment

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/...
```

Copy `deploy/aegis-serve.service`, set env in `/etc/aegisgo.env`, done —
see WORKFLOW.md.

[microsoft/agent-framework-go]: https://github.com/microsoft/agent-framework-go
[mark3labs/mcp-go]: https://github.com/mark3labs/mcp-go
[modernc.org/sqlite]: https://gitlab.com/cznic/sqlite
[Ollama]: https://ollama.com
