# ADR-0004: HITL decisions live in the Telegram transport, not the router

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: The approvals ledger (ADR-0003) needs a human surface. The
  repo already has an allowlisted Telegram face with claim-then-send
  idempotency — the natural place a human already is.
- **Decision**:
  1. `/approvals`, `/approve <id>`, `/deny <id>` (+ bare `/approve` = decide
     oldest pending) are dispatcher-level commands, handled BEFORE the
     engine path — deliberately unreachable as router rules. The router
     must never be able to approve anything (separation of powers with
     ADR-0002's fixed-argv catalog).
  2. Approval identity is `telegram:<chat_id>` — the allowlist is the
     authentication boundary (unknown chats are already never answered).
  3. The dispatcher depends on a narrow `approver` interface (2 methods),
     nil-safe: deployments without a store report "unavailable" instead
     of breaking.
  4. No-op outcomes (already decided / expired / unknown id) are surfaced
     honestly to the operator — silent success is a lie that erodes trust.
- **Consequences**: REST/gRPC HITL endpoints remain future work (same
  ledger, new transports). Inline keyboard buttons (tap instead of type)
  are the obvious UX follow-up; the text commands are the stable contract
  they will collapse onto.
