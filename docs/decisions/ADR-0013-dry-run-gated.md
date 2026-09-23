# ADR-0013: Dry-run gated verdicts (DescribeGated)

Date: 2026-09-23
Status: accepted

## Context
RunGated (ADR-0005) couples two things: rehearsing the approval decision
and executing the action. Before enabling real L2 commands, an approver
needs a way to practice the ✅/🚫 flow and inspect the exact payload that
WOULD run — without side effects.

## Decision
`tools.DescribeGated` reuses the RunGated loop verbatim (same ledger row,
same poll cadence, same fail-closed outcomes) but replaces the action with
a payload echo. On approval it returns the payload string; on deny/expiry/
timeout it returns the outcome with an empty payload.

## Consequences
- The approval row is decided either way, so the audit trail records
  rehearsals as first-class decisions (they show up in /history).
- No new state: dry-run is a usage pattern of the existing ledger, not a
  ledger flag — an approved dry-run does NOT entitle a later real run.
