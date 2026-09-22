# ADR-0011: Reload diff preview

- **Status**: Accepted
- **Date**: 2026-09-22

## Context

/reload_rules asked the owner to approve a PROMISE ("reload rules")
blind — the approval message never showed WHICH rules change. Owner
trust rule: approve what you see.

## Decision

`ReloadDiffGate` wraps `ReloadGate` (ADR-0007). Before creating the
approval row it loads the rules the reload WOULD install
(router.LoadRules) and diffs them against the live set
(Router.RuleDefs). The diff rides the existing reason field:

- `+ name → tool` (new rule)
- `− name` (rule dropped)
- `~ name` (pattern/tool/args changed)
- `no rule changes` when sets are identical

The push → buttons → edit pipeline is untouched: the diff is just text
in the reason. `gatedText` now takes a `gateHandler` interface so the
diff gate slots in without touching telegram.

## Consequences

- Zero new transports/tables: the preview costs one extra rules-table
  read at request time.
- The diff is a point-in-time snapshot; the executing reload re-reads
  the table — if rows change between request and approval the executed
  set may differ (acceptable: approvals are decided in seconds; the
  ledger still records what ran).
- Same pattern applies to every future gated action: preview belongs
  in the approval message, not in a separate command.
