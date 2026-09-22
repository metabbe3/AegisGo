# ADR-0003: HITL approval ledger in the store

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: Blueprint §K4 + §L: some actions (L2 tier) must pause for
  a human decision before executing. The repo already has every primitive
  this needs — durable SQLite, single-writer batcher, Telegram face —
  but no ledger to hold pending decisions.
- **Decision**:
  1. New `approvals` table (migration v4): kind, payload (opaque JSON),
     reason, state `pending → approved | denied | expired`, decided_by,
     decided_at. TEXT RFC3339 timestamps, matching the audit convention.
  2. `DecideApproval` transitions only `pending` rows (CAS via
     `WHERE state='pending'`); double-decide and decide-after-expiry are
     idempotent no-ops (returns false, never errors).
  3. `ExpireApprovals(ttl)` sweeper flips stale pending rows to expired —
     nothing waits forever.
  4. The store NEVER executes anything: it is the ledger only. The
     executor re-validates the action against policy at run time —
     approval is authorization, not validation.
  5. `CreateApproval` uses `INSERT … RETURNING id` on the pooled
     connection: an enqueued batcher write cannot return LastInsertId,
     and :memory: SQLite is per-connection — this keeps v0 synchronous
     and correct. Revisit if write volume demands batching.
- **Consequences**: interface wiring (Telegram buttons, REST endpoints,
  executor loop) can now be built against a stable ledger contract.
  Approved ≠ safe: policy re-check at execution time stays mandatory
  (fail-closed, ADR-0002 lineage).
