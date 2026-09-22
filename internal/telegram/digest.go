package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Digest sends the daily morning report (ADR-0010): one message, the
// numbers an owner needs at a glance — uptime, last decisions, pending
// count, deflection. Deterministic: no LLM anywhere (policy: bot text
// is always deterministic).
type Digest struct {
	client  Client
	chatIDs []int64
	stats   statser
	hist    historian
	pend    approver
	started time.Time
	logger  *slog.Logger
}

// NewDigest builds the digest sender. Any nil source degrades its line
// honestly instead of failing the whole message.
func NewDigest(c Client, chatIDs []int64, stats statser, hist historian,
	pend approver, logger *slog.Logger) *Digest {
	if logger == nil {
		logger = slog.Default()
	}
	return &Digest{client: c, chatIDs: chatIDs, stats: stats, hist: hist,
		pend: pend, started: time.Now(), logger: logger}
}

// Send renders and delivers today's digest to every chat.
func (dg *Digest) Send(ctx context.Context) {
	text := dg.text(ctx)
	for _, chat := range dg.chatIDs {
		if _, err := dg.client.SendMessage(ctx, chat, text, 0); err != nil {
			dg.logger.Error("telegram: digest send failed", "chat_id", chat, "error", err)
		}
	}
}

// text assembles the digest body; every section is optional so a dead
// source cannot blank the whole report.
func (dg *Digest) text(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("☀️ Morning digest\n")
	up := time.Since(dg.started).Round(time.Second)
	fmt.Fprintf(&b, "uptime %s\n", up)

	if dg.stats != nil {
		if snap, err := dg.stats.Stats(ctx); err == nil && snap != nil {
			fmt.Fprintf(&b, "runs %d · deflection %.1f%%\n",
				snap.TotalRuns, snap.DeflectionRate*100)
			if avg, ok := snap.AvgLatencyMS["regex_router"]; ok {
				fmt.Fprintf(&b, "router answers in %dms\n", avg)
			}
		}
	}

	if dg.pend != nil {
		if pend, err := dg.pend.PendingApprovals(ctx, 50); err == nil {
			if len(pend) == 0 {
				b.WriteString("no approvals waiting 🎉\n")
			} else {
				fmt.Fprintf(&b, "⏳ %d approval(s) waiting — /approvals\n", len(pend))
			}
		}
	}

	if dg.hist != nil {
		if dec, err := dg.hist.RecentDecisions(ctx, 3); err == nil && len(dec) > 0 {
			b.WriteString("last decisions:\n")
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
				fmt.Fprintf(&b, "%s #%d %s\n", icon, a.ID, humanKind(a.Kind))
			}
		}
	}
	return b.String()
}
