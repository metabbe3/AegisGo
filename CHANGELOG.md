# Changelog

All notable changes to AegisGo. Format: Keep-a-Changelog; this project
adapts it to agent-org merges (every entry = one feature branch merged).

## [Unreleased]

### Added
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
