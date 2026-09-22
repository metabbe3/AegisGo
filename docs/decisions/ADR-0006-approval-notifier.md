# ADR-0006: Proactive approval notifications

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: HITL only works if the human KNOWS something is waiting.
  Asking the approver to poll /approvals is an inverted responsibility —
  the system should come to the human (owner: "kalau perlu approval bisa
  langsung kirim ke sana").
- **Decision**:
  1. `telegram.Notifier` polls the ledger (default 5s) and pushes every
     NEW pending approval to the allowlisted chats with the id, kind,
     reason, payload preview and one-tap /approve · /deny lines.
  2. Announce-once semantics via highest-seen-id; rows pending at boot
     are primed as seen (boot is not a reason to spam old requests).
  3. Works in poll AND webhook mode — it reads the store, not the
     transport. Empty chat list = clean disable. Send failures are
     logged and retried next tick; the loop never dies.
  4. The notifier NEVER decides anything: read-only ledger access + one
     SendMessage per approval per chat.
- **Consequences**: approval latency drops from "whenever the human
  checks" to ~5s + reaction time. Poll cadence is a knob, not a belief;
  a future SQLite hook or LISTEN/NOTIFY could replace polling without
  touching the announce contract.
