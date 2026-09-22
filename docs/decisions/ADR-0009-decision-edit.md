# ADR-0009: Decision-time message editing

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: Pushed approval messages kept their ✅/🚫 buttons after
  the decision landed. The chat accumulated stale buttons and reply
  lines; a press hours later was an honest no-op but LOOKED actionable.
- **Decision**:
  1. EditRegistry records (approvalID → chat, messageID) at push time
     (the notifier already owns that moment).
  2. On a successful decide (any path this process sees: button, typed
     command), Dispatcher.OnDecided enqueues a final-state edit
     ("✅ #3 approved by telegram:100 · 13:07 UTC") — buttons disappear.
  3. Editor is a single drain worker; edit failure logs and moves on —
     the ledger is truth, the edit is polish. REST decisions made from
     other processes simply have no recorded target (no-op).
- **Consequences**: chat history reads as a decision log; stale-button
  confusion is structurally gone; ~120 lines, 4 tests.
