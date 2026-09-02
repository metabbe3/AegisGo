# WORKFLOW.md — How Work Happens Here

## Everyday Loop

1. Read CLAUDE.md rules + PRODUCT.md scope. If a change fights the scope
   boundaries, stop and discuss before coding.
2. Branch or worktree off main (keep main always deployable).
3. Make the change: internal/ logic first, thin cmd/ wiring, then tests.
4. `make vet && make test` — green or it doesn't ship.
5. Commit with a `why:`-oriented message; note any dependency bumps.

## Task Recipes

### Add a router rule

Rules live in the SQLite `rules` table (seeded from
`internal/router/seeded.go` on first boot) and hot-reload every
`AEGIS_RULES_RELOAD` seconds (default 30; 0 disables). Two ways:

- **At runtime**: `sqlite3 aegisgo.db "INSERT INTO rules (name,pattern,tool,args_template,origin,enabled,created_ts) VALUES ('disk_free', '/diskfree', 'system_command', '{\"command\":\"disk\"}', 'seed', 1, datetime())"`
  — live within one reload interval, no restart.
- **In code** (persistent for fresh deployments): edit `Seeded()`, delete
  `aegisgo.db`'s rules or INSERT manually; keep specific patterns before
  general ones (first match wins).

Rules of rules: anchored automatically; capture groups splice as `"$1"`
(quoted, escaped) or bare `$1` (only when the regex validates the shape,
e.g. `(\d+)`); the referenced tool must exist or startup fails loudly.

### Add a builtin tool

1. Create `internal/tools/<name>tool.go`: define `In`/`Out` structs (field
   comments become the schema description the LLM reads), build with
   `tools.New(Config{...}, typedHandler)`.
2. Append it in `internal/tools/registry.go` — never reorder existing tools.
3. File access goes through `resolvePath`; cap returned size.
4. Tests: happy path, bounds, workspace-escape rejection, and dual-entry
   parity (same args → same result via `Execute` and `FuncTool().Call`).

### Create dirs, download files & background jobs

Seeded rules route `/mkdir <path>`, `/ls [path]`, `/download <url> <path>`
and `/job <id>` to the `make_dir`, `list_dir`, `download` and `job_status`
tools; the LLM can call the same tools by name on any phrasing. Knobs:

- `AEGIS_DOWNLOAD_TIMEOUT` (seconds, default 120) — end-to-end bound.
- `AEGIS_DOWNLOAD_MAX_BYTES` (default 64 MiB) — oversize fails and the
  partial file is removed.
- `AEGIS_DOWNLOAD_ALLOW_PRIVATE=on` — allow loopback/private URLs. Off by
  default so a deployed agent can't be steered at internal endpoints;
  enable for local/e2e runs.

Downloads return a `job_id` immediately and run on a background goroutine
(detached from the request context, bounded by the timeout). Poll
`/job <id>` (or the `job_status` tool) until status leaves `"running"`.
Two caveats by design: jobs are per-process — poll from the same
`aegis serve` process, not a fresh CLI invocation — and existing databases never
grow new seeded rules (fresh DB or INSERT per the recipe above).

### Add a system-command catalog entry

Edit `catalog` in `internal/tools/systemtool.go`: fixed argv, no
user-controlled flags, unix command that terminates on its own. If a
useful command needs arguments, the *rule* or the *LLM* selects between
several fixed-argv variants (see `disk` vs a hypothetical `disk_inodes`) —
input still never reaches argv. Update the tool description string and the
REPL banner in internal/cli (`aegis agent`).

### Point sql_query at a real database

Default is the embedded SQLite file (read-only). To query an external DB:

1. `go get github.com/jackc/pgx/v5/stdlib` (or your driver), import it
   blank in `internal/tools/sqltool.go`.
2. Set `AEGIS_SQL_DSN=postgres://...`. `?`-placeholder policy applies;
   read-only enforcement falls back to the statement-prefix allowlist, so
   consider a read-replica DSN.

### Attach an external MCP server

No code:

```bash
export AEGIS_MCP_SERVERS="stdio:/usr/local/bin/some-mcp-server,https://mcp.internal:8443/mcp"
```

Remote tools join the LLM's toolset (they are not router-addressable —
rules reference builtin tools only, by design). Startup fails fast if any
server is unreachable.

### Add an LLM provider

1. Constant + validation branch in `internal/config`.
2. A case in `internal/provider.New`. Everything above (router, tools,
   MCP, serving) is provider-blind.
3. Smoke-test with real creds in a shell (`AEGIS_*`), never in committed
   tests.

### Regenerate the gRPC stubs

Only after editing `internal/pb/agent.proto` (stubs are committed —
ordinary builds need no protoc):

```bash
make proto    # protoc + protoc-gen-go[-grpc] (~/go/bin)
go build ./... && go test ./internal/grpcapi/
```

### Operate the self-mining lifecycle

```bash
aegis ctl rules mine --threshold 20   # manual pass (or AEGIS_MINER_INTERVAL hourly)
aegis ctl rules list                  # state: active | shadow | demoted
aegis ctl rules promote mined_1       # manual override (auto needs a streak)
aegis ctl rules demote mined_1        # kill a misbehaving rule
aegis ctl stats                       # deflection rate, top fallback shapes
aegis ctl replay <trace-id>           # one run's audit trail
```

Rules of the lifecycle: only path-arg tools are auto-mined
(read_csv/csv_stats/read_doc); shadow rules never answer (the LLM does,
while tool-choice agreement is compared); divergence demotes instantly;
promoted rules keep a permanent 1% sampled double-check. If a promoted rule
goes wrong in production: `aegis ctl rules demote <name>` — the next hot
reload drops it (≤ AEGIS_RULES_RELOAD seconds).

### Enable the Telegram bot

Everything ships built-in; activation is env-only:

```bash
export AEGIS_TELEGRAM_TOKEN=…            # @BotFather
export AEGIS_TELEGRAM_CHATS=424242       # numeric chat IDs, comma-separated
# webhook mode (public TLS ingress):
export AEGIS_TELEGRAM_WEBHOOK_URL=https://bot.example.com
# else: long-poll mode (zero ingress, works behind NAT)
```

A bad token fails boot loudly (getMe). Empty allowlist boots but skips
everything (WARN at startup) — set the allowlist before pointing users at
it. To rotate the webhook secret: restart with a new
`AEGIS_TELEGRAM_WEBHOOK_SECRET` (or none — a fresh one is generated and
re-registered each boot). Inbox ops:

```bash
sqlite3 aegisgo.db "SELECT status, COUNT(*) FROM telegram_inbox GROUP BY 1"
sqlite3 aegisgo.db "SELECT update_id, status FROM telegram_inbox ORDER BY update_id DESC LIMIT 10"
```

Dev/demo without a real bot: point `AEGIS_TELEGRAM_API_BASE` at a fake
Bot API and POST updates to `/telegram/webhook` yourself (see
internal/telegram/telegram_test.go for the shapes).

### Operate the audit trail

```bash
sqlite3 aegisgo.db "SELECT interface, decision_source, COUNT(*), AVG(latency_ms) FROM audit_events GROUP BY 1,2"
sqlite3 aegisgo.db "SELECT normalized_prompt, COUNT(*) c FROM fallback_events GROUP BY 1 ORDER BY c DESC LIMIT 10"  # Phase-3 mining preview
```

The answers table TTLs rows out (~15 min) and lazily deletes on read.

### End-to-end verification (macOS)

Run the staged e2e before tagging a release, after touching the router,
tools, store, or any interface wiring — it exercises the real binaries
(`make build` runs inside it), not test doubles:

```bash
make e2e        # or ./scripts/e2e.sh — needs go, sqlite3, curl, python3
```

Twelve stages, each proving one slice: S1 CLI router matrix · S2 workspace
csv/log/doc reads plus a hot-reloaded `/search` rule (sql_query attach_csv)
· S3 REST matrix (sync, async 202→poll, SSE, /v1/stats, 404, X-Trace-Id
echo) · S4 hot reload · S5 gRPC via grpcurl · S6 MCP server attach ·
S7 Telegram against a python3 fake Bot API (poll mode + webhook with
secret check) · S8 aegis ctl · S9 miner · S10 audit-trail joins · S11 real
LLM fallback through local Ollama, MCP echo tool included.

Notes:

- **Darwin caveat, asserted**: `free` does not exist on macOS, so `/memory`
  and `/free` are *expected to fail* with `command "memory" failed`. If
  they ever succeed on a Mac, the system_command catalog has drifted.
- S5 and S11 are optional-but-run-when-present: without grpcurl or an
  Ollama model they print `SKIP` with the fix (`brew install grpcurl`,
  `ollama pull qwen2.5:0.5b`) and the run still passes.
- Fixed ports 18080-18083 + 18090, guarded up front; a busy port means a
  previous run is still alive. Each run gets its own mktemp workspace, so
  back-to-back runs are safe.
- The script starts (and kills) everything it needs on loopback; if no
  Ollama server is up it starts one for the run only.

### Bump dependencies

`mcp-go`: prefer stable tags. `agent-framework-go`: pinned to a commit (no
tags upstream); after bumping, check constructor signatures against
`reference/agent-framework-go`, run the full gate plus one real run.
`modernc.org/sqlite`: rerun `TestConcurrentAuditNoBusy` (the batcher gate).

## Release / Deploy (Linux)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/...
scp bin/aegis user@host:~/  # unit ExecStart runs: aegis serve
```

`deploy/aegis.service` is a systemd unit template. Router-only
degradation: set `AEGIS_LLM=off` in the unit and the service survives
provider outages (deterministic commands keep answering). Keep provider
env vars in `EnvironmentFile=/etc/aegisgo.env`, never in the repo.

## Reference Repos

`make clone-refs` shallow-clones both upstream libraries under
`reference/` (gitignored, read-only): consult their examples when framework
APIs churn; never import from `reference/`.

## Commit / PR Conventions

- Subject: imperative, ≤72 chars. Body: what + why; mention env var or
  behavior changes.
- Dependency bumps are their own commit.
- Anything touching `resolvePath`, the system_command catalog, SQL policy,
  or the store's batcher deserves an extra careful re-read — those are the
  security and data-integrity boundaries.
