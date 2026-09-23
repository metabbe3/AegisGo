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
	// CreatedAt is RFC3339 from the ledger; empty = unknown age (no
	// reminders for sources that do not fill it).
	CreatedAt string
}

// Notifier pushes new pending approvals to chats.
type Notifier struct {
	// edits records pushed-message targets for decision-time editing
	// (ADR-0009); nil = editing disabled.
	edits *EditRegistry

	src      ApprovalSource
	client   Client
	chatIDs  []int64
	interval time.Duration
	logger   *slog.Logger

	// lastSeen is the highest approval id announced (0 = nothing yet).
	// Rows only arrive here via CreateApproval (autoincrement), so
	// id ordering is insertion ordering.
	lastSeen int64

	// reminderAfter: re-announce a still-pending approval once it is older
	// than this. reminderAt tracks the next due reminder per approval id;
	// each reminder pushes the next one a full reminderAfter later.
	reminderAfter time.Duration
	reminderAt    map[int64]time.Time
}

// NewNotifier builds a notifier. interval <= 0 defaults to 5s.
// An empty chat list disables it (Start returns immediately).
// SetEditRegistry attaches the decision-edit registry (ADR-0009).
func (n *Notifier) SetEditRegistry(r *EditRegistry) { n.edits = r }

func NewNotifier(src ApprovalSource, c Client, chatIDs []int64,
	interval time.Duration, logger *slog.Logger) *Notifier {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{src: src, client: c, chatIDs: chatIDs,
		interval: interval, logger: logger,
		reminderAfter: 30 * time.Minute, reminderAt: map[int64]time.Time{}}
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
		if a.ID > n.lastSeen {
			n.lastSeen = a.ID
			n.announce(ctx, a)
			continue
		}
		n.maybeRemind(ctx, a)
	}
}

// maybeRemind re-announces an approval that has sat pending past
// reminderAfter. Approvals with no CreatedAt never remind (unknown age).
func (n *Notifier) maybeRemind(ctx context.Context, a ApprovalInfo) {
	if n.reminderAfter <= 0 || a.CreatedAt == "" {
		return
	}
	created, err := time.Parse(time.RFC3339, a.CreatedAt)
	if err != nil {
		return // tolerate non-RFC3339 ledger formats: skip, don't crash
	}
	due, ok := n.reminderAt[a.ID]
	if !ok {
		due = created.Add(n.reminderAfter)
		n.reminderAt[a.ID] = due
	}
	if time.Now().After(due) {
		n.reminderAt[a.ID] = time.Now().Add(n.reminderAfter)
		n.announceReminder(ctx, a)
	}
}

// announceReminder re-sends the approval card with a nudge header. Same
// buttons as the original so deciding from the reminder edits the newest
// message (ADR-0009 registry keys by approval id, not message id).
func (n *Notifier) announceReminder(ctx context.Context, a ApprovalInfo) {
	n.announce(ctx, a)
	n.logger.Info("telegram: approval reminder sent", "approval_id", a.ID, "age", n.reminderAfter)
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
		msgID, err := n.client.SendMessageWithButtons(ctx, chat, b.String(), buttons)
		if err != nil {
			// Log and continue to the next chat; never lose the loop.
			n.logger.Error("telegram: approval notify failed",
				"chat_id", chat, "approval_id", a.ID, "error", err)
			continue
		}
		// Remember where the buttons live so the decision can edit this
		// exact message (ADR-0009).
		if n.edits != nil {
			n.edits.Record(a.ID, chat, msgID)
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
	var b strings.Builder
	b.WriteString("#")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteString(" · ")
	b.WriteString(humanKind(a.Kind))
	if reason != "" {
		b.WriteString("\n")
		b.WriteString(reason)
	}
	if line := renderPayload(a.Payload); line != "" {
		b.WriteString("\n")
		b.WriteString(line)
	}
	b.WriteString("\n/approve ")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteString(" · /deny ")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	return b.String()
}

// PrimeForTest primes synchronously (tests need determinism; production
// primes inside Start).
func (n *Notifier) PrimeForTest(ctx context.Context) { n.prime(ctx) }

// TickForTest runs one poll pass synchronously.
func (n *Notifier) TickForTest(ctx context.Context) { n.tick(ctx) }
