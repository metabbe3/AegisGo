# ADR-0010: Daily digest

- **Status**: Accepted
- **Date**: 2026-09-22

## Context

The bot was reactive: it waits for approvals, and says nothing about
overall health. The owner runs several orgs; every one of them ends in
a morning digest because it is the cheapest trust signal — one message
that proves the system is alive and summarises what it did.

## Decision

`telegram.Digest` sends ONE deterministic message per chat every day at
07:00 local time:

- uptime (process start)
- total runs + deflection % + router latency (statser)
- pending approvals count or "no approvals waiting 🎉" (approver)
- last 3 decisions with verdict icons (historian)

Rules:
1. **No LLM, ever** — the digest is assembled from store queries; bot
   text stays deterministic (policy since ADR-0005).
2. **Degrade honestly per section** — a nil/dead source blanks its own
   line, never the whole digest.
3. First digest fires at the NEXT 07:00 after process start; interval
   is 24h from that alignment.
4. Timer lives in the serve goroutine group (shuts down with tgCtx).

## Consequences

- The owner gets a daily heartbeat with zero new infrastructure — the
  same Telegram client, the same store.
- Digest time is compile-time fixed (07:00 local); if a configurable
  time is ever needed it becomes an AEGIS_* env knob (rule #8).
- Send failures are logged, not retried — the next digest is 24h away
  by design; a retry loop would spam on a long outage.
