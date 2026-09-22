# Handoff — AegisGo Agent Org

## 2026-09-22 (session 1 — org bootstrap)

- **State**: repo cloned + verified on Mac mini (build/vet/test/cover/
  check all green, coverage 94.3%). Branch `feat/docs-foundation` active.
- **Done**: docs/ foundation — SDLC.md (process), ADR-0001 (mandate),
  this handoff. CHANGELOG.md seeded. Owner rules recorded: branch-per-
  feature invariant, push+merge to main, full SDLC without cron gating.
- **In-flight**: feat/docs-foundation — commit + push + merge to main.
- **Traps**: binary names are `aegis-agent` + `aegis-serve` (deck said
  one binary / four faces — CLAUDE.md is the truth). docs/ didn't exist
  until this branch. Deck claims "13 e2e gates" — scripts/e2e.sh exists,
  full staged run not yet executed on this machine (macOS supported).
- **Next first step**: L-tier policy engine (backlog #3) on a fresh
  branch after this one merges.

## 2026-09-22 (session 2 — e2e baseline)

- **Done**: `bash scripts/e2e.sh` full staged run on Mac mini:
  **S0-S10 PASS (11/11 runnable), S11 SKIP** (no ollama model —
  3090 PC off; needs `ollama pull qwen2.5:0.5b` to enable).
  RESULT: PASS, no failed stages. This is the machine baseline.

## 2026-09-22 (session 3 — policy engine v0)

- **Done**: feat/policy-engine — tier field on commandSpec, PolicyTier in
  SystemOutput + exported lookup, 2 new tests (L1-only invariant, tier in
  run output), ADR-0002. Gates: vet/test/check/cover all green.
- **Trap learned**: make check forbids t.Skipf — hostname test must fail
  honestly, not skip (project law, not a style choice).
- **Next first step**: K4 HITL approval path (L2 tier executor with
  pause/resume) — or N1 YAML manifest, pick per blueprint backlog.

## 2026-09-22 (session 4 — HITL ledger v0)

- **Done**: feat/hitl-gate — approvals table (v4 migration) + store CRUD
  with pending-only CAS, TTL expiry sweeper, capped pending list. 6 tests
  green; vet/test/check/cover(93.8%) all pass.
- **Trap learned**: modernc :memory: is per-connection — Open(":memory:")
  pins pool to 1 conn; scan TEXT timestamps into string, not time.Time
  (audit convention). INSERT..RETURNING for ids outside the batcher.
- **Next first step**: wire the ledger — engine L2 executor stub
  (create approval → wait → run if approved) + Telegram /approve /
  /deny commands. REST endpoints after.

## 2026-09-22 (session 5 — Telegram HITL wiring)

- **Done**: feat/telegram-approvals — /approvals, /approve <id>, /deny <id>,
  bare /approve = oldest. approver interface (2 methods), nil-safe; app.go
  passes the store; 8 dispatcher tests; help + WORKFLOW recipes; ADR-0004.
  Gates green (vet/test/check/cover 93.3%).
- **Design lock**: HITL commands are transport-level, NEVER router rules —
  the router cannot approve anything (separation of powers, ADR-0002/4).
- **Jev note**: owner asked "pastikan jev function ditambahkan jika
  berguna". Useful slice identified: typed verdicts (L1/L2/L3) +
  pending-only CAS already ARE jev-style constrained decisions. Deferred:
  logprob-confidence POC until a real decision point needs it.
- **Next first step**: L2 executor loop (engine side): tool run hits L2 →
  CreateApproval → poll DecideApproval → execute after policy re-check.
