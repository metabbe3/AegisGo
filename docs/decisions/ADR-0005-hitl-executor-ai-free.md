# ADR-0005: HITL gate executor + AI-free command replies

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: Two owner directives the same evening: (1) K4 needs the
  execution half — a ledger nobody waits on is a list, not a gate;
  (2) "pastikan telegram reply jika bisa tanpa AI" — command replies and
  listings must never burn an LLM call.
- **Decision**:
  1. `tools.RunGated` (internal/tools/hitl.go): create approval → poll
     ledger → execute ONLY on approved. Typed `GateOutcome`
     (approved/denied/expired/timeout/shutdown) — no ambiguous strings.
     The approved fn receives exactly the payload the approver saw.
  2. Fail-closed everywhere: deny, expiry, timeout, ctx-cancel all mean
     "do not run". Fn errors propagate with outcome=approved (the human
     said yes; the action itself failed — two different facts).
  3. AI-free guard in the dispatcher: unknown slash-commands get the
     deterministic help text, never the LLM fallback. Known router
     commands (via the /rules listing) fall through to their rule.
     With no rules listing wired the guard is conservative (pass-through)
     — availability beats false "unknown" answers.
  4. Store adapter pending: *store.Store satisfies ApprovalLedger via a
     thin shim (GetApproval returns store.Approval; the gate wants
     ApprovalRow) — wiring lands with the first gated tool.
- **Consequences**: every reply a user can trigger by TYPING a command is
  now deterministic and free; the LLM remains reachable only through
  free-form text. The 2-minute default gate window is a knob
  (GateConfig), not a constant belief.
