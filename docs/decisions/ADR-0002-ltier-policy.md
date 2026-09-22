# ADR-0002: L-tier command policy verdicts on system_command

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: AegisGo will run on production servers where it can touch
  system state. Blueprint §L (owner 22 Sep) defines a three-tier policy:
  L1 auto-allow (logged), L2 needs-approval (HITL gate), L3 hard-deny.
  The existing fixed-argv catalog is already an allowlist, but verdicts
  were implicit — nothing recorded or enforced a tier.
- **Decision**:
  1. Every catalog entry carries an explicit `tier` field; output and a
     new `PolicyTier(key)` report it.
  2. **Catalog membership == L1, enforced by test**: an entry with tier
     L2/L3 fails `TestPolicyTierReportedAndL1Only`. Commands needing
     approval will get a separate approval path (K4 HITL) — they will
     NOT be silently added to this catalog.
  3. L3 exists as documentation + review convention: prohibited actions
     (credential reads, destructive fs) are simply never catalog keys,
     and code review + this ADR are the guard.
- **Consequences**: verdicts become auditable data (joinable with audit
  rows via trace_id); adding an L2 command requires touching two places
  (approval mechanism + ADR update) which is intentional friction.
  Unknown keys keep returning "unknown command" — fail closed.
