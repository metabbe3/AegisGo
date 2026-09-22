# Changelog

All notable changes to AegisGo. Format: Keep-a-Changelog; this project
adapts it to agent-org merges (every entry = one feature branch merged).

## [Unreleased]

### Added
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
