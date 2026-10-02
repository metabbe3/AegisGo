## [2026-10-02] — merge #56 feat/sched-persist

- Durable schedules (SQLite v8): `/every` jobs now persist in `scheduled_jobs` and rehydrate at boot — restarts and nightly deploys no longer silently wipe them. Found two bugs in one audit: schedules were memory-only AND `Scheduler.Start()` was never called anywhere, so the feature degraded on every restart.
- Restart semantics: next run = now + interval (never a catch-up burst); rows whose chat fell off the allowlist or sit below the minute floor stay in the DB but don't launch — the allowlist remains the boundary.
- Register/Unregister are store-backed (honest failure replies on storage errors); `/scheduled` lists live jobs.
- Gates: vet, 25/25 pkgs, make check, coverage 90.0%.

## 2026-10-01 — merge #52 feat/watch-containment

- Hard Rule 2+10 debt closed on the logwatch family: `/watch` and `/analyze` now refuse kernel pseudo-filesystems (/dev /proc /sys) and non-log extensions at parse time; `tailFile` and `Analyze` refuse non-regular files as defense-in-depth (a legacy stored watch pointing at /dev/zero can never feed size-based allocation); `Analyze` reads an 8 MiB tail window instead of slurping whole files.
- Also fixes the serve.go gofmt debt from #54/#55 (gofmt -l clean again).

## 2026-09-29 — merge #53 feat/stats-window
- store: StatsWindow(days) — audit + fallback-corpus aggregates windowed by ts (RFC3339Nano TEXT vs datetime('now','-N days')); rules stay global (current state); StatsSnapshot.window_days
- REST: GET /v1/stats?days=N (1-3650, default all-time); ctl: `aegis ctl stats days=N`
- Telegram: `/stats days=N` prefix arm (exact-case can't shadow args); morning digest now windows runs/deflection to last 24h ("runs (24h)") — cumulative totals hid "what happened since yesterday"
- why: all-time totals read as static vanity numbers once the trail grows; windows make /stats answer real ops questions (is deflection holding THIS week)

## 2026-09-28 — merge #50 feat/health-command

- `/health` (Telegram): the agent audits its own answer quality — error share, avg confidence, avg latency per decision source, plus an unknown-source catch-all. Backed by `store.Health()` (outcome-aware GROUP BY over audit_events; confidence averages include pre-#44 rows as 0 — an honest signal of the unscored trail, documented in-code). Wired via the narrow-interface pattern of record (healther + SetHealth, honest degrade when unwired). Help + README synced.

## 2026-09-27 — merge #43 feat/mcp-http

- `aegis mcp-server --http <addr>`: MCP streamable-HTTP transport (ADR-0015) — same tool registry as stdio, stateless mode, for remote MCP clients. Gated by REQUIRED `AEGIS_MCP_TOKEN` (constant-time Bearer compare, 401 before any JSON-RPC parses; empty token refuses to serve — unlike `AEGIS_HTTP_TOKEN`, this surface can drive `system_command`). stdio unchanged, token-free. Verified end-to-end with a real mcp-go streamable-HTTP client: initialize → tools/list → tools/call round-trip.

## 2026-09-27 — chore/db-hygiene

- Untracked the stray `aegisgo.db` from git (a one-shot `aegis agent` run had seeded a live DB into the repo root — tracked AND modified in-tree) and added `*.db`/`*.db-shm`/`*.db-wal` to .gitignore. Guard test `scripts/gitdb_test.go` fails if any SQLite file ever reappears in `git ls-files`. The real DB lives outside the repo via `AEGIS_DB_PATH`.

## 2026-09-27 (ops, agent-utama)

- PROD LLM ON (opsi A, owner approval): AEGIS_LLM=on, provider anthropic → https://api.z.ai/api/anthropic, smart=glm-5.3, fast=glm-5.3-flash, classifier=on. launchd plist updated + service restarted.
- Binary deployed: be89c9d → **5bf6bb4** (main; menutup deploy-lag merges #38-40 — Evolution #17/#18).
- FIX saat switch: 401 dari z.ai → akar masalah dua token di ~/.hermes/.env; yang valid utk z.ai = ANTHROPIC_API_KEY (49ch). AUTH_TOKEN (119ch) = 401, dihapus dari plist. Recipe dicatat di aegisgo.env comments.
- VERIFY E2E: /uptime → regex_router 19ms zero-cost tetap; prompt bebas → decision_source=llm glm-5.3 via z.ai 11.5s OK; healthz/readyz ok; WIP feat/job-cancel di-restore utuh (stash pop).
## 2026-09-25 — merge #36 feat/sse-events
- `GET /v1/events`: SSE live run feed — one RunEvent per completed engine run (decision_source, trace_id, rule_id, latency_ms). Slow subscribers drop events; the run path never blocks on a dashboard. Heartbeat comment every 15s defeats idle-proxy reaping. Unwired = 503 like other /v1 routes.
- `engine.WrapRun`: middleware hook around Run (one wrapper max; double-wrap panics). The engine stays SSE-unaware — serve installs the publisher.
- Live-verified: subscribe + POST /v1/agent/run → `event: run {"decision_source":"regex_router",...latency_ms:16}` pushed <1s. Coverage 90.0% gate green.

## 2026-09-25 — merge #35 feat/ctl-jobs
- `GET /v1/jobs` + `aegis ctl jobs` (HTTP client via AEGIS_ADDR, bearer when AEGIS_HTTP_TOKEN set).

## 2026-09-25 — glm flash verified on Z.ai anthropic-compat
- One-shot live test: AEGIS_PROVIDER=anthropic + ANTHROPIC_BASE_URL=api.z.ai/api/anthropic + AEGIS_MODEL=glm-5.3-flash → rc 0, 4.6s; tier pair (smart glm-5.3 + fast flash + AEGIS_CLASSIFIER=on) works. Serving stays AEGIS_LLM=off (router-only) — switch is a deliberate operator decision, documented here.

## 2026-09-25 — merge #35 feat/ctl-jobs
- `GET /v1/jobs`: live background-job snapshot (running first, then finished newest-first, bounded by retention). Nil-safe: idle server returns `[]` not `null`.
- `aegis ctl jobs`: CLI client that asks the running server via `AEGIS_ADDR` (default http://localhost:8080); attaches `Authorization: Bearer` when `AEGIS_HTTP_TOKEN` is set.
- Coverage 90.0% (gate green). Tests: List ordering + adapter, endpoint 503/200, ctl client (table/empty/bearer/down).

# Changelog

## 2026-09-30 — merge #55 feat/dashboard-health

- Dashboard (GET /) self-audit card: total runs + error share headline, avg confidence per decision source below — the page-level mirror of /v1/health (#54). Card omitted when Health unwired/errors/zero-runs: the dashboard stays a liveness surface first, no misleading "0 runs · 0% err" on fresh installs.

## 2026-09-30 — merge #54 feat/rest-health

- `GET /v1/health`: REST mirror of the Telegram /health self-audit (merge #50) — error share, avg confidence, avg latency per decision source, in the standard envelope. Nil-wired = 503 like other optional routes; serve wires a.Store. Same contract as chat so scripts/dashboards don't need Telegram.
- Deploy note: repo binary now built WITH ldflags commit (footer "build dev" era ends) — aegisgo_start.py restart verified live.

## Added
- **2026-09-23 — Optional bearer auth on /v1/\* (AEGIS_HTTP_TOKEN)**: constant-time Bearer gate for the whole API surface; empty = open (LAN default). /healthz /readyz and the dashboard stay credential-free so uptime checks never break. Enables safe exposure beyond localhost.
- **2026-09-23 — /stats Telegram alias**: `/stats` now renders the same one-glance health payload as `/status` (runs, deflection, per-source split, rules, build) — one code path, zero duplication.
- **2026-09-23 — MCP server mode (ADR-0014)**: `aegis mcp-server` exposes the native tool registry over MCP stdio — same tools the router serves, callable from Claude Desktop or any MCP client. Adapter maps Tool.Execute(rawJSON) to a single `input` JSON-string schema.
- **2026-09-23 — Mini web dashboard at GET /**: self-contained HTML status page (uptime, runs/deflection, per-source breakdown, rules by state, build commit), meta-refresh 30s, no JS frameworks. Nil-stats still renders — liveness first.
- **2026-09-23 — Dry-run gated verdicts (ADR-0013)**: `tools.DescribeGated` rehearses the HITL flow with zero side effects — same approval row + verdict, action replaced by payload echo.
- **2026-09-23 — Approval TTL reminders**: still-pending approvals re-announce after 30 min (and every 30 min after), same ✅/🚫 buttons — decided-from-reminder edits the newest card (ADR-0009 keys by approval id). Sources without CreatedAt never remind.
- **2026-09-23 — Build commit in /status**: `internal/version` package + ldflags injection; /status now answers "which build is live" without shell access.
- **2026-09-23 — SQLite sidecar backup (ADR-0012)**: daily `VACUUM INTO` snapshot at 04:30 next to the DB, retention 7 files. `backupOnce` in `internal/app/backup.go` (idempotent same-day reruns, prune best-effort). Tests: sidecar is a readable DB, retention pruning, slice-bounds guard.

All notable changes to AegisGo. Format: Keep-a-Changelog; this project
adapts it to agent-org merges (every entry = one feature branch merged).

## [Unreleased]

### Added
- 2026-09-22 — /reload_rules diff preview (ADR-0011): approval reason
  embeds the old→new rule changes (+ added · − dropped · ~ changed) —
  approve what you see.
- 2026-09-22 — Daily digest (ADR-0010): one deterministic 07:00 message
  per chat — uptime, runs, deflection, pending count, last 3 decisions.
- 2026-09-22 — /history command: last 10 decisions with verdict icons,
  newest first.
- 2026-09-22 — Human-readable replies (owner rule): approval pushes,
  listings, decision edits, and /status labels render as sentences —
  no raw JSON, no underscores ("Command reload rules", "Router 1101",
  "✅ Approval #7 approved by Telegram · 14:13 UTC"). Unknown payload
  shapes degrade gracefully; 5 renderer tests pin it.

### Added
- 2026-09-22 — /status bot command (P4): one-glance health — uptime,
  total runs, deflection %, per-source counts, rules by state, router
  latency. Degrades honestly when stats aren't wired.

### Fixed
- 2026-09-22 — Inline buttons did NOTHING on tap (P0, owner-reported):
  allowed_updates lacked callback_query so Telegram never delivered
  button presses. Fixed in long-poll AND webhook; regression test
  pinned (LL-008).

### Added
- 2026-09-22 — Decision-time message editing (ADR-0009): pushed button
  messages are rewritten to "✅/🚫 #N verdict by X" on decide — stale
  buttons vanish, chat reads as a decision log. 4 tests.
- 2026-09-22 — Docs sync: README/WORKFLOW/CLAUDE/docs-README now cover
  buttons, 3 decision paths, /reload_rules, launchd ops, ADR-0001…0008.
- 2026-09-22 — HITL REST endpoints (ADR-0008): GET /v1/approvals +
  POST /v1/approvals/{id}/decision — same CAS as buttons/commands;
  409-with-truth on double-decide; 5 tests.
- 2026-09-22 — Merged ALL branches incl. upstream refactor/simplify-pass
  (13 Sep): e2e 13-stage, pathutil coverage, poll goroutine fixes,
  single-binary consolidation (aegis serve) — plist + deploy migrated.
- 2026-09-22 — First gated L2 action /reload_rules (ADR-0007):
  ReloadGate (app layer) + telegram.GatedAction interface; command →
  approval → ✅/🚫 buttons → execute-on-approve; deny/timeout keeps
  previous rules. 3 tests (approve/deny/timeout flows).
- 2026-09-22 — docs/org-memory: lessons-learned.md (6 LL seed: 409
  double-poller, guard-swallow, notifier false-alarm, KeepAlive dict,
  gateway scanner, fake-contract) + plans.md (P1-P4 roadmap + done list)
  + SDLC documentation rule (every merge touches lessons/plans/handoff).
- 2026-09-22 — Interactive approval buttons (feat/approval-buttons):
  notifier now sends ✅ Approve / 🚫 Deny inline keyboards; presses flow
  through the same inbox (cb_id/cb_data columns, migration v5) and the
  same pending-only CAS — answerCallbackQuery toast, typed fallback for
  every failure mode. 5 new tests.
- 2026-09-22 — launchd service (chore/launchd-service):
  scripts/install-launchd.sh installs aegis-serve as com.aegisgo.serve
  (RunAtLoad + KeepAlive + 30s throttle; binary at ~/.hermes/bin; token
  stays in the env file, never the repo). Live-verified: kill →
  auto-restart in <30s, zero 409 after restart.
- 2026-09-22 — Notifier observability + Build-level wiring test
  (fix/notifier-observability): prime/announce INFO logs, tick DEBUG,
  announce-success line; integration test proves Build() wires a working
  notifier end-to-end (fake Bot API). Root-caused live "silence": v1 race
  prime-vs-seed + user approving before log check — wiring was correct.
- 2026-09-22 — Proactive approval notifications (feat/hitl-notify):
  new pending approvals are pushed to allowlisted Telegram chats within
  ~5s (announce-once, boot primes as seen, transport-independent,
  never-decides) (ADR-0006). 3 tests.
- 2026-09-22 — HITL gate executor + AI-free command replies
  (feat/hitl-executor): `tools.RunGated` (typed outcomes, fail-closed,
  payload-exactly-as-approved) + dispatcher guard so unknown slash-commands
  answer deterministically and NEVER hit the LLM (ADR-0005). 8 gate tests.
- 2026-09-22 — Telegram HITL commands (feat/telegram-approvals):
  /approvals, /approve <id>, /deny <id>, bare /approve = oldest pending.
  Transport-level (never router rules), pending-only CAS, honest no-op
  surfacing, nil-store degradation (ADR-0004). 8 dispatcher tests.
- 2026-09-22 — HITL approval ledger (feat/hitl-gate): `approvals` table
  (migration v4) + store API Create/Get/Decide/Expire/Pending; pending-only
  CAS transitions, idempotent double-decide, TTL sweeper, store never
  executes (ADR-0003). 6 tests.
  (why: blueprint §K4/§L — L2 actions need a durable human gate)
- 2026-09-22 — L-tier policy verdicts on system_command (feat/policy-engine):
  every catalog entry carries an explicit tier (`L1`), reported in tool
  output + `tools.PolicyTier(key)`; catalog membership == L1 enforced by
  test (ADR-0002). Unknown keys stay fail-closed.
  (why: blueprint §L owner mandate — auditable command policy)
- 2026-09-22 — docs foundation (agent-org): `docs/agent-org/SDLC.md`
  (branch-per-feature pipeline + gates), `docs/decisions/ADR-0001`
  (agent-org takes over development), `docs/agent-org/handoff.md`
  (session continuity). CHANGELOG.md itself seeded on the same branch.
  (why: docs = source of truth, blueprint J; owner mandate 22 Sep)

## [2026-09-28] Decision Engine — confidence level per run (merge #44)

Owner directive 28 Sep: "make decision engine also like confidence level".
Every run now carries a deterministic confidence score (NOT an LLM judge —
per Evolution research 28 Sep, deterministic verification beats judge
models), mirroring the Hermes verifier ladder: HIGH ≥80 ship as-is,
MEDIUM 60-79 ship with note, LOW <60 verify before trusting.

Scoring by decision_source:
- regex_router 100 (deterministic rule + native tool)
- llm_classifier 90 (model-picked, deterministic tool output)
- llm 55 base + verifiable signals: +12 two-plus distinct tool calls
  (grounded), +6 one call, +10 cites a router rule, +8 router coverage
  exists for the family, +4 structured answer. Capped 100.
- llm_disabled / error 0 (refusal/failure, not an answer)

Surfaces:
- Result.Confidence — Header() shows "LOW conf=55" only for non-HIGH
  (HIGH stays bare; unscored zero-value stays bare — pinned by tests)
- REST /v1/agent/run JSON: confidence + confidence_band fields; SSE final
  event carries both
- audit_events.confidence column (migration v6; legacy rows = 0)
- slog request line includes confidence

Tests: confidence_test.go bands/caps/distinct-tools/header-contract;
existing header tests extended. Coverage 90.2% ≥ gate. make check clean.

## [2026-09-28] API envelope + /addrule from Telegram (merge #45)

Owner directive 28 Sep: "make the json response like API response with
error code and reason" + "add rules/command from telegram".

1) Standard envelope on every /v1 endpoint (envelope.go):
   {success, code, message?, reason?, data?}. code = stable token
   (OK/BAD_REQUEST/UNAUTHORIZED/NOT_FOUND/INTERNAL...), reason =
   failure category (auth/request/not_found/state/provider/internal).
   Engine errors classify as provider with the failure text as message.
   /healthz & /readyz stay plain (orchestrator probes). All handler
   tests updated to unwrap the envelope (decodeInto helper).

2) /addrule gated L2 action (addrulegate.go, ADR-0007 pattern):
   /addrule name=X | pattern=/x(?:\s+(?<arg>.*))? | tool=read_doc |
   args={"path":"docs/$arg"} — strict parse BEFORE any approval row
   (name charset, pattern must be slash-anchored anti-catch-all, regex
   must compile, args must be JSON); tool allowlist read_csv/csv_stats/
   read_doc (Hard Rule 12 symmetry — chat cannot widen the command
   surface); approve → INSERT origin=manual + router hot-swap; deny/
   timeout → nothing changes. Help text updated.

Gates: vet clean, 22/22 pkgs ok, coverage 90.0% (gate 90), make check
clean. jsonField helper removed (dead code caught by the gate itself).


## [2026-09-28] 24/7 log watchdog + markdown replies (merge #46)

Owner directive 28 Sep: "train it to read log files and find patterns,
monitor 24h like a watchdog", "add/remove watches from Telegram, no
rebuild", "analyze logs for patterns", "reformat responses markdown",
"update docs".

1) internal/logwatch — one goroutine per watch (loop.Periodic), stat+tail
   polling with rotation detection, per-watch cooldown de-dup, bounded
   alerts. Definitions persist in SQLite log_watches (v7) and rehydrate
   at boot: /watch from chat is live instantly and survives restarts —
   no binary rebuild.
2) /analyze — deterministic pattern analysis: normalize masks UUID/IP/
   DUR/"STR"/[...]/digits/volatile keys (trace=abc → trace=N), tallies
   top templates + top error shapes + error rate over the last N lines.
   No LLM in the loop.
3) Telegram: SendMarkdown opt-in (our generated texts only — model
   answers stay plain so arbitrary text never hits the markup parser);
   /help redesigned into grouped markdown sections.
4) Makefile cover now uses -coverpkg across packages (cross-package
   adapter coverage was invisible; 89.8 → 90.1 honest).
5) README command reference + API envelope docs.

Gates: vet clean, 23/23 pkgs, coverage 90.1%, make check clean.


## [2026-09-28] Log analysis family (merge #47)

Owner directive 28 Sep: "analyze api performance, application exception,
access / audits logs, behaviours logs".

internal/loganalysis — five deterministic analyses over any log file (and
the agent's own audit_events for /audit): API perf (p50/p95/slowest/5xx),
exceptions (template grouping), access (IPs/routes/status/agents), audit
trail rollup (decision-source mix, conf avg, latency), behaviour (actions,
peak hours, bursts, novel shapes). Zero LLM anywhere.

Commands: /api_perf /exceptions /access /audit /behaviour (markdown).
Route normalization collapses volatile ids so per-endpoint stats group.
RE2 constraints honored (no lookahead/backref — manual post-vetting).

Gates: vet, 24/24 pkgs, coverage 90.0%, check.


## [2026-09-28] analyze_log tool + full wiring (merge #48)

Owner directive: "bikin kita command saja lewat telegram dia bisa wiring
semua itu ke code kita tanpa perlu kita suruh — seperti Hermes, cuma udah
ada tools bawaan".

- NEW TOOL analyze_log (10th builtin): kinds perf|exceptions|access|
  behaviour; dual-entry (Execute + FuncTool); paths go through the SAME
  resolvePath containment as read_csv/read_doc (Hard Rule 2 — closes the
  gap where the Telegram gates read files directly).
- Seeded router rules: /api_perf /exceptions /access /behaviour (+ -n N
  variants) — instant, zero LLM, from Telegram AND HTTP AND CLI.
- Telegram gates delegate to the tool via registry Execute: one code
  path, one security boundary.
- /addrule allowlist += analyze_log: new commands wireable from chat.
Gates: vet, 24/24 pkgs, coverage 90.0%, check.


## 2026-09-28 — merge #49 feat/ops-forecast
- internal/opsforecast: time-bucket a log's timestamps (1-60min windows), least-squares slope + EWMA, ETA-to-threshold, level + confidence (deterministic, no LLM)
- tools: ops_forecast (tool #11) — metric err_rate|req_rate|latency_p95, bounded output, recommended_action hints (scale-out candidate, page-now, watch)
- seeded rule /forecast → ops_forecast; coverage gate 90.1%
