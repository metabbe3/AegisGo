package telegram

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Approval notifier (ADR-0006): when the store gains a NEW pending
// approval, push it to the allowlisted chats so a human can /approve or
// /deny without polling. Polls the ledger — SQLite has no cheap pub/sub,
// and the cadence (default 5s) is negligible next to a human's reaction
// time. One notify per approval id, ever: the notifier remembers the
// highest id it announced and only pushes unseen, pending rows.

// ApprovalSource is the ledger slice the notifier needs.
type ApprovalSource interface {
	PendingApprovals(ctx context.Context, limit int) ([]ApprovalInfo, error)
}

// ApprovalInfo decouples the notifier from store.Approval.
type ApprovalInfo struct {
	ID      int64
	Kind    string
	Reason  string
	Payload string
}

// Notifier pushes new pending approvals to chats.
type Notifier struct {
	src      ApprovalSource
	client   Client
	chatIDs  []int64
	interval time.Duration
	logger   *slog.Logger

	// lastSeen is the highest approval id announced (0 = nothing yet).
	// Rows only arrive here via CreateApproval (autoincrement), so
	// id ordering is insertion ordering.
	lastSeen int64
}

// NewNotifier builds a notifier. interval <= 0 defaults to 5s.
// An empty chat list disables it (Start returns immediately).
func NewNotifier(src ApprovalSource, c Client, chatIDs []int64,
	interval time.Duration, logger *slog.Logger) *Notifier {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{src: src, client: c, chatIDs: chatIDs,
		interval: interval, logger: logger}
}

// Start runs until ctx is cancelled. Errors are logged and retried on the
// next tick — a transient Telegram failure must never kill notifications.
func (n *Notifier) Start(ctx context.Context) {
	if len(n.chatIDs) == 0 {
		n.logger.Info("telegram: approval notifier disabled (no chats)")
		return
	}
	t := time.NewTicker(n.interval)
	defer t.Stop()
	// Prime silently: treat everything already pending at boot as seen
	// (boot is not a reason to spam old requests).
	n.prime(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.tick(ctx)
		}
	}
}

func (n *Notifier) prime(ctx context.Context) {
	pend, err := n.src.PendingApprovals(ctx, 50)
	if err != nil {
		n.logger.Error("telegram: notifier prime failed", "error", err)
		return
	}
	for _, a := range pend {
		if a.ID > n.lastSeen {
			n.lastSeen = a.ID
		}
	}
	n.logger.Info("telegram: approval notifier primed", "last_seen", n.lastSeen)
}

func (n *Notifier) tick(ctx context.Context) {
	pend, err := n.src.PendingApprovals(ctx, 50)
	if err != nil {
		n.logger.Error("telegram: notifier poll failed", "error", err)
		return
	}
	n.logger.Debug("telegram: notifier tick", "pending", len(pend), "last_seen", n.lastSeen)
	for _, a := range pend {
		if a.ID <= n.lastSeen {
			continue
		}
		n.lastSeen = a.ID
		n.announce(ctx, a)
	}
}

func (n *Notifier) announce(ctx context.Context, a ApprovalInfo) {
	var b strings.Builder
	b.WriteString("🔔 Approval needed\n")
	line := formatApprovalLine(a)
	// Strip the trailing /approve · /deny hint — the buttons replace it.
	if i := strings.LastIndex(line, "\n/approve"); i >= 0 {
		line = line[:i]
	}
	b.WriteString(line)
	buttons := [][]Button{{
		{Label: "✅ Approve", Data: "apr:" + strconv.FormatInt(a.ID, 10)},
		{Label: "🚫 Deny", Data: "dny:" + strconv.FormatInt(a.ID, 10)},
	}}
	for _, chat := range n.chatIDs {
		if _, err := n.client.SendMessageWithButtons(ctx, chat, b.String(), buttons); err != nil {
			// Log and continue to the next chat; never lose the loop.
			n.logger.Error("telegram: approval notify failed",
				"chat_id", chat, "approval_id", a.ID, "error", err)
			continue
		}
		n.logger.Info("telegram: approval notified", "approval_id", a.ID, "chat_id", chat)
	}
}

// formatApprovalLine renders one approval compactly (shared with listings).
func formatApprovalLine(a ApprovalInfo) string {
	reason := a.Reason
	if len(reason) > 80 {
		reason = reason[:80] + "…"
	}
	payload := a.Payload
	if len(payload) > 120 {
		payload = payload[:120] + "…"
	}
	var b strings.Builder
	b.WriteString("#")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteString(" ")
	b.WriteString(a.Kind)
	b.WriteString(" — ")
	b.WriteString(reason)
	b.WriteString("\n")
	b.WriteString(payload)
	b.WriteString("\n/approve ")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteString(" · /deny ")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	return b.String()
}
