# CLAUDE.md — Working Rules for AI Agents on This Repo

Rules for Claude Code (and any AI assistant) contributing to AegisGo. Follow
these unless the user explicitly overrides them. See PRODUCT.md for what the
project is, WORKFLOW.md for how changes are made.

## Commands

```bash
make build        # compile bin/aegis-agent and bin/aegis-serve
make test         # go test ./...          — no network, no API keys needed
make vet          # go vet ./...
make cover        # unit tests with coverage gate (fails below 90%)
make check        # no TODO/FIXME, no skipped tests, every non-generated pkg tested
make e2e          # staged macOS end-to-end run (scripts/e2e.sh)
go run ./cmd/aegis-agent "/uptime"       # router hit: instant, zero LLM cost
AEGIS_LLM=off go run ./cmd/aegis-serve   # router-only mode, no credentials needed
go run ./cmd/aegis-serve                 # HTTP service on :8080
```

Run `make vet && make test` before declaring any change done. Both must pass.
Tests must never require network access or real API keys; fake providers or
the in-process MCP transport instead.

## Architecture Map

```
cmd/aegis-agent          CLI: one-shot prompt or interactive REPL
cmd/aegis-serve          HTTP service (see internal/server), graceful shutdown
internal/app             shared wiring: store → tools → registry → router → engine
internal/engine          hybrid pipeline: router first, LLM fallback, audit (§ decision_source)
internal/router          deterministic rules: anchored regex → tool Execute; hot reload
internal/tools           common Tool interface + read_csv, csv_stats, read_doc,
                         system_command (fixed-argv catalog), sql_query (ro by default)
internal/store           embedded SQLite: audit trail, rules table, fallback corpus,
                         async answers; single-writer batcher
internal/server          REST: /healthz /readyz /v1/agent/run (sync|?async=1|?stream=1 SSE) /v1/answers/{id} /v1/stats, optional webhook mount
internal/grpcapi         gRPC surface over the same engine (internal/pb holds the committed proto stubs)
internal/telegram        Telegram interface: stdlib Bot API client, dispatcher, durable inbox, webhook + long-poll transports
internal/miner           fallback corpus → shadow rules (pattern synthesis, dominance threshold)
internal/mcpclient       mark3labs/mcp-go client + adapter to framework tools
internal/provider        env-switched agent factory (openai | openai-compat | anthropic | foundry)
internal/trace           trace-ID minting/propagation (joins logs, audit rows, answers)
internal/config          env parsing, validation, fast/smart model tiers
examples/mcp-echo-server reference for the MCP SERVER side (mcp-go)
testdata/                fixtures used by unit tests
reference/               gitignored shallow clones of upstream libs — read, never import
```

Request flow (every interface): trace.New → engine.Run → router (regex match?
execute tool natively, decision_source=regex_router) → else LLM fallback
(decision_source=llm, event persisted as Phase-3 mining corpus) → audit row +
slog line. `AEGIS_LLM=off` makes misses return fast (llm_disabled) — the
router keeps serving with zero credentials.

## Hard Rules

1. **Stdlib-first.** One exception so far: `modernc.org/sqlite` (pure Go,
   keeps CGO_ENABLED=0). Any other dependency needs a stated reason in the
   commit text.
2. **Workspace path safety is a security boundary.** Every file-reading path
   goes through `internal/tools.resolvePath` (symlink-aware, escape-proof).
   Never bypass it. This applies to the SQL tool's attach_csv too.
3. **system_command is fixed-argv, forever.** Input selects a catalog KEY
   only; user/model text never reaches argv (defeats find -exec / awk
   system() escapes). New commands = new catalog entries in
   internal/tools/systemtool.go, edited by a human, flags included.
4. **sql_query is read-only by default.** `mode=ro` connection (engine-level
   enforcement) + statement-prefix allowlist + single-statement only +
   string-literals-must-be-placeholders. `AEGIS_SQL_MODE=rw` is the explicit
   opt-in. Don't weaken one layer because another exists.
5. **All store writes go through the single-writer batcher.** Audit helpers
   (`Audit`, `RecordFallback`) enqueue and never block the request path.
   Only rare admin writes (rule seeding) use the blocking `Exec`.
6. **Every run reports a decision_source.** `regex_router`, `llm`,
   `llm_disabled`, `error` — in the slog line AND the audit row, keyed by
   trace_id. New code paths keep this contract.
7. **Tools implement the common interface** (`internal/tools/tool.go`):
   `Execute(ctx, json.RawMessage)` for deterministic callers, `FuncTool()`
   for the LLM. One typed handler, two entry points, same result (there's a
   parity test — keep it passing).
8. **Configuration is env-only.** New knobs go in `internal/config` with an
   `AEGIS_*` name, a default, and validation if applicable.
9. **Router rules are anchored + order-sensitive.** Specific patterns
   (optional groups present) precede general ones; first match wins. Bare
   `$N` arg splices are only allowed when the regex validates the capture
   shape (e.g. `\d+`).
10. **Bounded tool output.** Cap anything a tool returns (row caps, byte
    caps) — unbounded reads blow the model context and RAM.
11. **Telegram delivery invariants.** Webhook acks BEFORE processing
    (ack-then-process); replies are claimed atomically on update_id
    (claim-then-send — see inbox.go); the long-poll offset advances only
    past processed rows (process-then-ack). Two tests pin these
    (TestCrashWindowExactlyOneReply, TestOffsetOrdering) — a change that
    breaks them is wrong, not the tests. Unknown chats are never answered.
    The Bot API client stays stdlib-only; do not add a Telegram SDK.
12. **Mining invariants.** Shadow comparison is tool-choice agreement
    (engine.ToolNames), never answer-text comparison. Divergence ALWAYS
    demotes; promoted mined rules ALWAYS keep the 1% sampling
    (router.sampleActiveMined) — the guard that keeps falling LLM spend
    from rewarding wrong answers. The miner only proposes path-arg tools
    (read_csv/csv_stats/read_doc) — system_command and sql_query are never
    auto-mined. Generated proto stubs are committed; regenerate only via
    `make proto`.

## Pinned Dependencies & Known Risks

- `github.com/mark3labs/mcp-go v0.58.0` — stable tag. Prefer tags over @main.
- `github.com/microsoft/agent-framework-go v0.0.0-20260901091852-de5a4c162072`
  — **public preview, no release tags**; pinned to a commit. Upstream API
  churn is the top breakage risk; check `reference/agent-framework-go` for
  new signatures (tool interface, provider constructors, RunText/Collect).
- `modernc.org/sqlite v1.57.0` — pure-Go SQLite; transpiled, heavier than
  CGo but keeps the single static binary. Concurrent-write behavior is
  covered by `TestConcurrentAuditNoBusy` (the go/no-go gate for the batcher).

## Style

- Go 1.26, standard `gofmt` formatting, package comments on internal packages.
- Errors wrap with `%w`, start lowercase, and name the env var when config
  is the cause.
- Comments explain *why* — especially around security, concurrency, and
  provider quirks.
- Tests use fakes (fakeEngine, fakeLLM, in-process MCP, :memory: SQLite);
  no test ever dials a provider.

## Definition of Done (per change)

1. `make vet && make test` green.
2. New behavior covered by a test that fails without the change.
3. Router-rule or tool changes update the seeded-rules help text (cmd
   repl banner) and WORKFLOW.md recipes.
4. No secrets, no `.env` files, no `reference/` imports in code.
