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

### Add a system-command catalog entry

Edit `catalog` in `internal/tools/systemtool.go`: fixed argv, no
user-controlled flags, unix command that terminates on its own. If a
useful command needs arguments, the *rule* or the *LLM* selects between
several fixed-argv variants (see `disk` vs a hypothetical `disk_inodes`) —
input still never reaches argv. Update the tool description string and the
repl banner in cmd/aegis-agent.

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

### Bump dependencies

`mcp-go`: prefer stable tags. `agent-framework-go`: pinned to a commit (no
tags upstream); after bumping, check constructor signatures against
`reference/agent-framework-go`, run the full gate plus one real run.
`modernc.org/sqlite`: rerun `TestConcurrentAuditNoBusy` (the batcher gate).

## Release / Deploy (Linux)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/...
scp bin/aegis-serve user@host:~/
```

`deploy/aegis-serve.service` is a systemd unit template. Router-only
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
