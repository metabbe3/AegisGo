## 2026-09-25 — merge #35 feat/ctl-jobs
- `GET /v1/jobs`: live background-job snapshot (running first, then finished newest-first, bounded by retention). Nil-safe: idle server returns `[]` not `null`.
- `aegis ctl jobs`: CLI client that asks the running server via `AEGIS_ADDR` (default http://localhost:8080); attaches `Authorization: Bearer` when `AEGIS_HTTP_TOKEN` is set.
- Coverage 90.0% (gate green). Tests: List ordering + adapter, endpoint 503/200, ctl client (table/empty/bearer/down).

# Changelog

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
