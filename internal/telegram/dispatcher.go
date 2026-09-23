package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
	"aegisgo/internal/version"
)

// Telegram hard limits: 4096 chars per message; we cap lower and keep room
// for the truncation notice.
const maxReplyLen = 4000

// placeholderText is sent immediately for non-command prompts so the user
// sees acknowledgment while the LLM runs.
const placeholderText = "…"

// Dispatcher is the transport-free core: allowlist → command handling →
// hybrid engine → reply. Both the webhook and long-poll transports feed the
// same inbox, and the worker pool drives this. Replies are idempotent on
// update_id — at-least-once delivery never double-sends.
type Dispatcher struct {
	engine engineRunner
	client Client
	inbox  *Inbox
	// allow is the chat allowlist; nil denies everything (AEGIS_TELEGRAM_CHATS
	// unset). Secure default: an unknown chat must never steer the agent.
	allow map[int64]bool
	// rules renders the /rules listing.
	rules  func() []string
	logger *slog.Logger
	// approver backs the HITL commands; nil disables them.
	approver approver
	// gated maps "/name" → L2 action flows (nil = none wired).
	gated map[string]GatedAction
	// stats backs /status (nil = command reports unavailable).
	stats statser
	// hist backs /history (nil = command reports unavailable).
	hist historian
	// startedAt anchors the /status uptime line.
	startedAt time.Time
	// onDecided fires after a successful decision (edit pushed message).
	onDecided func(approvalID int64, verdict, by string)

	mu      sync.Mutex
	buckets map[int64]*chatBucket
	sched   *Scheduler
}

// engineRunner is the slice of *engine.Engine the dispatcher needs.
type engineRunner interface {
	Run(ctx context.Context, prompt string) engine.Result
}

// approver is the slice of *store.Store the dispatcher needs for HITL
// commands (/approvals, /approve, /deny). Nil disables them.
type approver interface {
	PendingApprovals(ctx context.Context, limit int) ([]store.Approval, error)
	DecideApproval(ctx context.Context, id int64, state, decidedBy string) (bool, error)
}

// NewDispatcher builds the core. rules may be nil (then /rules says so).
// ap may be nil — HITL commands are then reported as unavailable.
func NewDispatcher(e engineRunner, c Client, inbox *Inbox,
	allowChats []int64, rules func() []string, logger *slog.Logger, ap approver) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	allow := make(map[int64]bool, len(allowChats))
	d := &Dispatcher{startedAt: time.Now()}
	for _, id := range allowChats {
		allow[id] = true
	}
	return &Dispatcher{
		engine: e, client: c, inbox: inbox, allow: allow,
		rules: rules, logger: logger, buckets: make(map[int64]*chatBucket),
		sched:    NewScheduler(e, c, allowChats, logger),
		approver: ap, startedAt: d.startedAt,
	}
}

// Process handles one inbox row: the single path from update to reply.
func (d *Dispatcher) Process(ctx context.Context, row InboxRow) {
	// Deny by default: unknown chats are skipped, not answered — answering
	// would confirm the bot exists to an attacker.
	if row.ChatID == 0 || !d.allow[row.ChatID] {
		if err := d.inbox.MarkStatus(ctx, row.UpdateID, InboxSkipped); err != nil {
			d.logger.Error("telegram: marking denial", "error", err)
		}
		d.logger.Warn("telegram: update from non-allowlisted chat skipped",
			"chat_id", row.ChatID, "update_id", row.UpdateID)
		return
	}

	if strings.TrimSpace(row.Text) == "" && row.CallbackData == "" {
		_ = d.inbox.MarkStatus(ctx, row.UpdateID, InboxSkipped)
		return
	}

	// Button presses: callback_data "apr:<id>" / "dny:<id>". Same pending-
	// only CAS as the text commands, then ack the query (stops the spin
	// animation) — the outcome lands as the toast + an edited message.
	if row.CallbackData != "" {
		d.claimAndSend(ctx, row, d.callbackText(ctx, row))
		return
	}

	// /every takes arguments — prefix-match before the exact switch.
	if strings.HasPrefix(row.Text, "/every ") {
		d.claimAndSend(ctx, row, d.sched.Register(row.ChatID, row.Text))
		return
	}

	switch row.Text {
	case "/help", "/start":
		d.claimAndSend(ctx, row, d.helpText())
		return
	case "/rules":
		d.claimAndSend(ctx, row, d.rulesText())
		return
	case "/approvals":
		d.claimAndSend(ctx, row, d.approvalsText(ctx))
		return
	case "/status", "/stats":
		d.claimAndSend(ctx, row, d.statusText(ctx))
		return
	case "/history":
		d.claimAndSend(ctx, row, d.historyText(ctx))
		return
	case "/scheduled":
		d.claimAndSend(ctx, row, d.sched.List())
		return
	case "/unschedule":
		d.claimAndSend(ctx, row, d.sched.Unregister(row.Text))
		return
	case "/deny":
		d.claimAndSend(ctx, row, d.decideText(ctx, row, "denied"))
		return
	case "/approve":
		d.claimAndSend(ctx, row, d.decideText(ctx, row, "approved"))
		return
	}

	// HITL prefixed forms: "/approve 3", "/deny 3" (id first word after the
	// command). Kept OUT of the engine path — approvals are transport-level,
	// never router rules (the router must not be able to approve anything).
	if id, rest, ok := strings.Cut(strings.TrimPrefix(row.Text, "/"), " "); ok {
		state := ""
		switch id {
		case "approve":
			state = "approved"
		case "deny":
			state = "denied"
		}
		if state != "" {
			d.claimAndSend(ctx, row, d.decideIDText(ctx, row, state, rest))
			return
		}
	}

	// Gated L2 actions ("/reload_rules"): create approval + wait + run on
	// approve (ADR-0007). The reply reports the typed outcome — honest for
	// every branch, including timeout and deny.
	cmd0 := strings.TrimSpace(strings.SplitN(row.Text, " ", 2)[0])
	if ga, ok := d.gated[cmd0]; ok {
		d.claimAndSend(ctx, row, ga.HandleText(ctx, row.Text))
		return
	}

	// Unknown slash-commands NEVER reach the LLM (owner rule 22 Sep: replies
	// and listings must be AI-free). Known router commands (from the rules
	// listing) fall through to the engine and answer via their rule; anything
	// else gets the deterministic help text instead of a fallback call.
	if strings.HasPrefix(row.Text, "/") && !d.knownRouterCommand(row.Text) {
		d.claimAndSend(ctx, row, d.unknownCommandText(row.Text))
		return
	}

	// Idempotency first: claim the reply atomically. A redelivery, a second
	// worker, or a post-crash replay all lose the race here and stop.
	claimed, err := d.inbox.ClaimReply(ctx, row.UpdateID)
	if err != nil {
		d.logger.Error("telegram: claiming reply", "update_id", row.UpdateID, "error", err)
		return
	}
	if !claimed {
		d.logger.Debug("telegram: update already replied", "update_id", row.UpdateID)
		return
	}

	// Command-shaped prompts (router candidates) run to completion first —
	// rule hits answer in milliseconds, so no placeholder is needed.
	// Free-form prompts get an instant placeholder, then an edit.
	if !strings.HasPrefix(row.Text, "/") {
		placeholderID, err := d.throttledSend(ctx, row.ChatID, placeholderText)
		if err != nil {
			d.logger.Error("telegram: sending placeholder", "error", err)
			d.release(ctx, row.UpdateID)
			return
		}
		_, rctx := trace.New(engine.WithIFace(ctx, store.IFaceTelegram), "")
		res := d.engine.Run(rctx, row.Text)
		body := formatReply(res)
		if err := d.client.EditMessageText(ctx, row.ChatID, placeholderID, body); err != nil {
			// Edit can fail (message too old, deleted); fall back to a new
			// message so the answer is never lost. The claim already stands.
			d.logger.Warn("telegram: editing placeholder failed, sending new message", "error", err)
			if _, err := d.throttledSend(ctx, row.ChatID, body); err != nil {
				d.logger.Error("telegram: fallback send", "error", err)
			}
		}
		return
	}

	_, rctx := trace.New(engine.WithIFace(ctx, store.IFaceTelegram), "")
	res := d.engine.Run(rctx, row.Text)
	if _, err := d.throttledSend(ctx, row.ChatID, formatReply(res)); err != nil {
		d.logger.Error("telegram: sending reply", "chat_id", row.ChatID, "error", err)
		d.release(ctx, row.UpdateID)
	}
}

// claimAndSend is the /help-style path: claim, then send, releasing the
// claim if the send fails outright (so a redelivery can retry).
func (d *Dispatcher) claimAndSend(ctx context.Context, row InboxRow, body string) {
	claimed, err := d.inbox.ClaimReply(ctx, row.UpdateID)
	if err != nil || !claimed {
		if err != nil {
			d.logger.Error("telegram: claiming reply", "update_id", row.UpdateID, "error", err)
		}
		return
	}
	if _, err := d.throttledSend(ctx, row.ChatID, body); err != nil {
		d.logger.Error("telegram: sending reply", "chat_id", row.ChatID, "error", err)
		d.release(ctx, row.UpdateID)
	}
}

// release undoes a claim when the send failed deterministically (API error,
// rate limit) so a redelivery gets another chance. One combined UPDATE, not
// MarkStatus + Unclaim: the half-released middle state (failed but still
// claimed) serves nobody.
func (d *Dispatcher) release(ctx context.Context, updateID int64) {
	if err := d.inbox.FailAndReopen(ctx, updateID); err != nil {
		d.logger.Error("telegram: releasing claim", "update_id", updateID, "error", err)
	}
}

// throttledSend respects the per-chat rate limit (~1 msg/s with a small
// burst — Telegram's documented guidance) before sending.
func (d *Dispatcher) throttledSend(ctx context.Context, chatID int64, text string) (int64, error) {
	if err := d.waitChat(ctx, chatID); err != nil {
		return 0, err
	}
	return d.client.SendMessage(ctx, chatID, text, 0)
}

// formatReply shapes an engine result as the Telegram body, mirroring the
// CLI's decision header so cost behavior is visible.
func formatReply(res engine.Result) string {
	body := res.Answer
	if body == "" {
		body = "(no output)"
	}
	if len(body) > maxReplyLen {
		body = body[:maxReplyLen] + "\n… (truncated)"
	}
	if res.RuleID != "" {
		return res.Header(" · ") + "\n" + body
	}
	return body
}

func (d *Dispatcher) helpText() string {
	return "AegisGo agent — hybrid answers.\n" +
		"Router commands answer instantly and free: /uptime /disk /memory /hostname /kernel /who\n" +
		"Schedule any command: /every 30m /disk · /scheduled · /unschedule #1\n" +
		"/csv_summary <path> · /csv_head <path> [rows]\n" +
		"/rules lists every active rule.\n" +
		"HITL: /approvals lists pending · /approve <id> · /deny <id> (bare /approve decides the oldest).\n/status (alias /stats) — one-glance health: runs, deflection, latency, rules.\n/reload_rules — L2 action: hot-reload router rules after approval (✅/🚫 buttons).\n" +
		"Anything else goes to the LLM (if enabled)."
}

func (d *Dispatcher) rulesText() string {
	if d.rules == nil {
		return "No rules listing available."
	}
	lines := d.rules()
	if len(lines) == 0 {
		return "No rules active."
	}
	return "Active rules:\n" + strings.Join(lines, "\n")
}

// historian is the decisions slice /history needs.
type historian interface {
	RecentDecisions(ctx context.Context, limit int) ([]store.Approval, error)
}

var _ historian = (*store.Store)(nil)

// statser is the stats slice /status needs (the store already
// implements it — stats.Snapshot via store.Stats).
type statser interface {
	Stats(ctx context.Context) (*store.StatsSnapshot, error)
}

var _ statser = (*store.Store)(nil)

// chatBucket is a tiny token bucket: burst 3, refill 1/s.
type chatBucket struct {
	tokens float64
	last   time.Time
}

const (
	bucketBurst   = 3.0
	bucketRefill  = 1.0 // tokens per second
	bucketMaxWait = 5 * time.Second
)

// waitChat blocks until the chat has a token or the ctx/max-wait expires.
func (d *Dispatcher) waitChat(ctx context.Context, chatID int64) error {
	d.mu.Lock()
	b, ok := d.buckets[chatID]
	if !ok {
		b = &chatBucket{tokens: bucketBurst, last: time.Now()}
		d.buckets[chatID] = b
	}
	d.mu.Unlock()

	deadline := time.Now().Add(bucketMaxWait)
	for {
		d.mu.Lock()
		now := time.Now()
		b.tokens = min(bucketBurst, b.tokens+now.Sub(b.last).Seconds()*bucketRefill)
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			d.mu.Unlock()
			return nil
		}
		need := (1 - b.tokens) / bucketRefill
		d.mu.Unlock()

		if time.Now().Add(seconds(need)).After(deadline) {
			return fmt.Errorf("chat %d rate limited; dropping", chatID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(seconds(need)):
		}
	}
}

// seconds converts a fractional second count to a Duration.
func seconds(f float64) time.Duration {
	return time.Duration(f * float64(time.Second))
}

// statusText renders /status: uptime + one-glance health from the stats
// snapshot (P4 item). Plain lines, no tables — Telegram-friendly.
func (d *Dispatcher) statusText(ctx context.Context) string {
	up := time.Since(d.startedAt).Round(time.Second)
	var b strings.Builder
	fmt.Fprintf(&b, "🩺 AegisGo status\nuptime %s\n", up)
	fmt.Fprintf(&b, "build %s\n", version.Commit)
	if d.stats == nil {
		b.WriteString("stats unavailable")
		return b.String()
	}
	snap, err := d.stats.Stats(ctx)
	if err != nil {
		b.WriteString("stats error: " + err.Error())
		return b.String()
	}
	fmt.Fprintf(&b, "runs %d · deflection %.1f%%\n", snap.TotalRuns, snap.DeflectionRate*100)
	if len(snap.BySource) > 0 {
		labels := map[string]string{
			"regex_router":   "Router",
			"llm_classifier": "Fast tier",
			"llm":            "LLM",
			"llm_disabled":   "LLM off",
			"error":          "Errors",
		}
		parts := make([]string, 0, len(snap.BySource))
		for _, src := range []string{"regex_router", "llm_classifier", "llm", "llm_disabled", "error"} {
			if n, ok := snap.BySource[src]; ok {
				parts = append(parts, fmt.Sprintf("%s %d", labels[src], n))
			}
		}
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	if len(snap.RulesByState) > 0 {
		parts := make([]string, 0, len(snap.RulesByState))
		for _, st := range []string{"seed", "active", "mined", "shadow"} {
			if n, ok := snap.RulesByState[st]; ok {
				parts = append(parts, fmt.Sprintf("%s %d", st, n))
			}
		}
		b.WriteString("rules: " + strings.Join(parts, " · ") + "\n")
	}
	if avg, ok := snap.AvgLatencyMS["regex_router"]; ok {
		fmt.Fprintf(&b, "Router answers in %dms\n", avg)
	}
	return b.String()
}

// SetHistory wires the decisions source for /history.
func (d *Dispatcher) SetHistory(h historian) { d.hist = h }

// historyText renders /history: the latest decisions, newest-first.
// Human lines only — verdict icon, what, who, when.
func (d *Dispatcher) historyText(ctx context.Context) string {
	if d.hist == nil {
		return "History unavailable (no store wired)."
	}
	dec, err := d.hist.RecentDecisions(ctx, 10)
	if err != nil {
		return "History lookup failed: " + err.Error()
	}
	if len(dec) == 0 {
		return "No decisions yet. Everything is still pending. ⏳"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📜 Last %d decisions (newest first):\n", len(dec))
	for _, a := range dec {
		icon := "•"
		switch a.State {
		case "approved":
			icon = "✅"
		case "denied":
			icon = "🚫"
		case "expired":
			icon = "⌛"
		}
		who := a.DecidedBy
		if i := strings.Index(who, ":"); i > 0 {
			who = strings.ToUpper(who[:1]) + who[1:i]
		}
		when := a.DecidedAt
		if t, err := time.Parse(time.RFC3339Nano, when); err == nil {
			when = t.UTC().Format("02 Jan 15:04 UTC")
		}
		fmt.Fprintf(&b, "%s #%d %s — by %s · %s\n", icon, a.ID, humanKind(a.Kind), who, when)
	}
	return b.String()
}
