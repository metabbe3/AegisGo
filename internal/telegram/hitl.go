package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"aegisgo/internal/store"
)

// HITL command rendering (ADR-0003 wiring). Everything here is read-only or
// a single pending-only CAS decide — the transport never executes anything.

// approvalsText lists pending approvals oldest-first, capped.
func (d *Dispatcher) approvalsText(ctx context.Context) string {
	if d.approver == nil {
		return "Approvals unavailable (no store wired)."
	}
	pend, err := d.approver.PendingApprovals(ctx, 10)
	if err != nil {
		return "Approvals lookup failed: " + err.Error()
	}
	if len(pend) == 0 {
		return "No pending approvals. 🎉"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pending approvals (%d):\n", len(pend))
	for _, a := range pend {
		reason := a.Reason
		if len(reason) > 80 {
			reason = reason[:80] + "…"
		}
		payload := a.Payload
		if len(payload) > 120 {
			payload = payload[:120] + "…"
		}
		fmt.Fprintf(&b, "#%d %s — %s\n  %s\n  /approve %d · /deny %d\n",
			a.ID, a.Kind, reason, payload, a.ID, a.ID)
	}
	return b.String()
}

// decideText handles bare "/approve" and "/deny": decide the OLDEST pending
// approval (lowest id). Errors include the id when one exists.
func (d *Dispatcher) decideText(ctx context.Context, row InboxRow, state string) string {
	if d.approver == nil {
		return "Approvals unavailable (no store wired)."
	}
	pend, err := d.approver.PendingApprovals(ctx, 1)
	if err != nil {
		return "Approvals lookup failed: " + err.Error()
	}
	if len(pend) == 0 {
		verb := "deny"
		if state == "approved" {
			verb = "approve"
		}
		return "Nothing pending to " + verb + "."
	}
	oldest := pend[0]
	return d.decideOne(ctx, oldest.ID, state, row)
}

// decideIDText handles "/approve <id>" and "/deny <id>".
func (d *Dispatcher) decideIDText(ctx context.Context, row InboxRow, state, rest string) string {
	if d.approver == nil {
		return "Approvals unavailable (no store wired)."
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "Usage: /approve <id> (see /approvals)."
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return "Not a valid approval id: " + rest
	}
	return d.decideOne(ctx, id, state, row)
}

// decideOne performs the CAS decide and reports the outcome honestly:
// no-op (already decided/expired) is surfaced as such, never as success.
func (d *Dispatcher) decideOne(ctx context.Context, id int64, state string, row InboxRow) string {
	by := fmt.Sprintf("telegram:%d", row.ChatID)
	ok, err := d.approver.DecideApproval(ctx, id, state, by)
	if err != nil {
		return fmt.Sprintf("Deciding #%d failed: %v", id, err)
	}
	if !ok {
		return fmt.Sprintf("#%d is not pending (already decided or expired) — nothing changed.", id)
	}
	// Decision landed: edit the pushed button message to its final state
	// (ADR-0009) — the chat stays a tidy record and stale buttons vanish.
	if d.onDecided != nil {
		d.onDecided(id, state, by)
	}
	switch state {
	case "approved":
		return fmt.Sprintf("#%d approved. The waiting executor (if any) proceeds after policy re-check.", id)
	default:
		return fmt.Sprintf("#%d denied.", id)
	}
}

// compile-time: the interface is satisfied by *store.Store.
var _ approver = (*store.Store)(nil)

// unknownCommandText answers unrecognized slash-commands deterministically
// (no LLM): acknowledge, show help, keep it short.
func (d *Dispatcher) unknownCommandText(cmd string) string {
	c := cmd
	if len(c) > 40 {
		c = c[:40] + "…"
	}
	return "Unknown command " + c + "\n\n" + d.helpText()
}

// knownRouterCommand reports whether a slash-command matches a live router
// rule (from the /rules listing the app wires in). Used to keep the
// AI-free guard from swallowing real commands like /uptime.
func (d *Dispatcher) knownRouterCommand(text string) bool {
	if d.rules == nil {
		// No listing wired: fall back to conservative behavior — pass
		// through to the engine (a rule miss then answers via the engine's
		// own deterministic path; with the LLM off nothing is spent).
		return true
	}
	cmd := strings.TrimSpace(strings.SplitN(text, " ", 2)[0])
	for _, line := range d.rules() {
		// listing format: "name → pattern → tool"
		parts := strings.Split(line, " → ")
		if len(parts) != 3 {
			continue
		}
		if strings.HasPrefix(parts[1], cmd) {
			return true
		}
	}
	return false
}

// callbackText handles inline-keyboard presses: "apr:<id>" / "dny:<id>".
// Answers the callback (toast) and returns the decision line as the reply.
func (d *Dispatcher) callbackText(ctx context.Context, row InboxRow) string {
	if d.approver == nil {
		return "Approvals unavailable (no store wired)."
	}
	data := strings.TrimSpace(row.CallbackData)
	state, idStr, ok := strings.Cut(data, ":")
	if !ok {
		return "Bad callback data: " + data
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return "Bad approval id in callback: " + idStr
	}
	var want string
	switch state {
	case "apr":
		want = "approved"
	case "dny":
		want = "denied"
	default:
		return "Unknown callback action: " + state
	}
	// Answer the press FIRST (spinner stops even if the decide is slow).
	if d.client != nil {
		_ = d.client.AnswerCallbackQuery(ctx, row.CallbackID, "Processing…")
	}
	outcome := d.decideOne(ctx, id, want, row)
	toast := "✅ Approved"
	if want == "denied" {
		toast = "🚫 Denied"
	}
	if strings.Contains(outcome, "not pending") {
		toast = "ℹ️ Nothing changed"
	}
	if d.client != nil {
		_ = d.client.AnswerCallbackQuery(ctx, row.CallbackID, toast)
	}
	return outcome
}

// GatedAction is an L2 action the human can trigger by command: it makes
// its own approval, waits for the verdict (typed), and runs only on
// approve. The dispatcher never learns the action's internals.
type GatedAction interface {
	// Handle runs the full gated flow; the string result is human text.
	HandleText(ctx context.Context, reason string) string
}

// SetStats wires the stats source for /status.
func (d *Dispatcher) SetStats(s statser) { d.stats = s }

// RegisterGated wires named actions ("/reload_rules" → action).
// Call once at build time; nil map = the path stays inert.
func (d *Dispatcher) RegisterGated(m map[string]GatedAction) {
	d.gated = m
}

// OnDecided registers the post-decision hook (edit the pushed message,
// ADR-0009). Safe to call once at build time.
func (d *Dispatcher) OnDecided(f func(approvalID int64, verdict, by string)) {
	d.onDecided = f
}
