# ADR-0008: HITL REST decision endpoints

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: Approvals had two entry paths (typed commands, inline
  buttons) — both human-Telegram. Scripts/CLI/dashboards had no way to
  list or decide approvals programmatically.
- **Decision**:
  1. `GET /v1/approvals` (limit ≤100) and
     `POST /v1/approvals/{id}/decision` {"decision":"approve"|"deny",
     "by":"identity"} on the existing server.Handler; Deps.Approvals
     (ApprovalSource) is optional — nil answers 503, not a silent 404.
  2. Same store CAS: double-decide → 409 + current row (the caller sees
     who won). Missing → 404, malformed → 400. Body capped 4KiB.
  3. `by` defaults to "rest"; the ledger records it — audit trail stays
     one table for all three paths.
  4. Scope: localhost service like every admin surface here; the chat
     allowlist is deliberately NOT applied to REST.
- **Consequences**: three decision paths, one truth: whichever path
  decides first wins; others get honest conflicts. e2e curl-able.
