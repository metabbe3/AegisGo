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

	mu      sync.Mutex
	buckets map[int64]*chatBucket
}

// engineRunner is the slice of *engine.Engine the dispatcher needs.
type engineRunner interface {
	Run(ctx context.Context, prompt string) engine.Result
}

// NewDispatcher builds the core. rules may be nil (then /rules says so).
func NewDispatcher(e engineRunner, c Client, inbox *Inbox,
	allowChats []int64, rules func() []string, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	allow := make(map[int64]bool, len(allowChats))
	for _, id := range allowChats {
		allow[id] = true
	}
	return &Dispatcher{
		engine: e, client: c, inbox: inbox, allow: allow,
		rules: rules, logger: logger, buckets: make(map[int64]*chatBucket),
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

	if strings.TrimSpace(row.Text) == "" {
		_ = d.inbox.MarkStatus(ctx, row.UpdateID, InboxSkipped)
		return
	}

	switch row.Text {
	case "/help", "/start":
		d.claimAndSend(ctx, row, d.helpText())
		return
	case "/rules":
		d.claimAndSend(ctx, row, d.rulesText())
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
// rate limit) so a redelivery gets another chance.
func (d *Dispatcher) release(ctx context.Context, updateID int64) {
	if err := d.inbox.MarkStatus(ctx, updateID, InboxFailed); err != nil {
		d.logger.Error("telegram: releasing claim", "update_id", updateID, "error", err)
	}
	if err := d.inbox.Unclaim(ctx, updateID); err != nil {
		d.logger.Error("telegram: unclaiming", "update_id", updateID, "error", err)
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
		header := fmt.Sprintf("[%s via %s · %dms]", res.DecisionSource, res.RuleID, res.LatencyMS)
		return header + "\n" + body
	}
	return body
}

func (d *Dispatcher) helpText() string {
	return "AegisGo agent — hybrid answers.\n" +
		"Router commands answer instantly and free: /uptime /disk /memory /hostname /kernel /who\n" +
		"/csv_summary <path> · /csv_head <path> [rows]\n" +
		"/rules lists every active rule.\n" +
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
