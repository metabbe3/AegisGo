# ADR-0007: First gated L2 action — /reload_rules

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: RunGated (ADR-0005) existed with no real consumer. The
  blueprint's L2 tier ("needs approval") was policy on paper only.
- **Decision**:
  1. `ReloadGate` (internal/app/reloadgate.go): /reload_rules re-reads
     the rules table and swaps the live router — runtime behavior change,
     squarely L2. Denied/expired/timeout keeps the previous rules (worst
     case = nothing changed, reported honestly).
  2. Flow: command → CreateApproval → notifier pushes ✅/🚫 buttons →
     RunGated polls the ledger → execute only on approved (10 min wait).
  3. Wiring respects the import graph: telegram exposes the
     GatedAction interface; app implements it; the dispatcher never
     learns action internals (transport stays generic).
  4. `existingApproval` adapter reuses RunGated's loop for an
     already-created row — one wait/verdict path, not two.
- **Consequences**: the K4 loop is closed end-to-end with a real action:
     command → approval → button → execution → audit. Adding the next
     L2 action = new ReloadGate-style type + one RegisterGated line
     (intentional, small friction).
