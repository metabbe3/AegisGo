package telegram

import (
	"context"
	"log/slog"
	"time"

	"aegisgo/internal/logx"
)

// Poll defaults.
const (
	pollTimeout  = 30 * time.Second // server-side long-poll hold
	pollMinBack  = time.Second
	pollMaxBack  = time.Minute
	pollBatchMax = 50
)

// PollLoop is the zero-ingress transport: getUpdates long-polling for boxes
// behind NAT. Delivery semantics: every fetched update is enqueued
// durably; the ack cursor (offset) advances only past update_ids at or
// below the highest fully processed one. Advancing first would destroy
// messages on a crash — process-then-ack, always.
type PollLoop struct {
	client Client
	inbox  *Inbox
	pool   *WorkerPool
	logger *slog.Logger
}

// NewPollLoop builds the poll transport.
func NewPollLoop(c Client, inbox *Inbox, pool *WorkerPool, logger *slog.Logger) *PollLoop {
	logger = logx.Or(logger)
	return &PollLoop{client: c, inbox: inbox, pool: pool, logger: logger}
}

// Run blocks until ctx is cancelled. It is the transport's only goroutine;
// workers live in the shared pool.
func (p *PollLoop) Run(ctx context.Context) {
	backoff := pollMinBack
	p.logger.Info("telegram: long-poll transport started (no public ingress required)")
	for {
		if ctx.Err() != nil {
			return
		}
		high, err := p.inbox.HighWater(ctx)
		if err != nil {
			p.logger.Error("telegram: reading high water", "error", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		updates, err := p.client.GetUpdates(ctx, high+1, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("telegram: getUpdates failed; backing off", "error", err, "backoff", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(pollMaxBack, backoff*2)
			continue
		}
		backoff = pollMinBack

		for _, u := range updates {
			known, err := p.inbox.Enqueue(ctx, u)
			if err != nil {
				p.logger.Error("telegram: enqueue failed", "update_id", u.UpdateID, "error", err)
				continue // leave unacked; next poll refetches (at-least-once)
			}
			if !known {
				p.logger.Debug("telegram: duplicate update ignored", "update_id", u.UpdateID)
			}
		}
		p.pool.Wake()

		// Advance the ack cursor only past fully processed work. Rows not
		// yet done stay below the cursor and get refetched after a crash.
		// An empty batch can never ack anything, so skip the Pending scan
		// idle polls would otherwise run every cycle.
		if len(updates) > 0 {
			if err := p.advance(ctx, updates); err != nil {
				p.logger.Error("telegram: advancing high water", "error", err)
			}
		}
	}
}

// advance moves the high-water mark to the highest update_id whose inbox
// row (and every earlier one we fetched) is done or terminally skipped.
// Gaps block advancement — conservative by design; Telegram tolerates a
// lagging offset fine.
func (p *PollLoop) advance(ctx context.Context, updates []Update) error {
	// Map processed state for the fetched batch.
	rows, err := p.inbox.Pending(ctx, pollBatchMax*10)
	if err != nil {
		return err
	}
	pending := make(map[int64]bool, len(rows))
	for _, r := range rows {
		pending[r.UpdateID] = true
	}

	var ackTo int64
	for _, u := range updates {
		if u.UpdateID <= 0 {
			continue
		}
		if pending[u.UpdateID] {
			break // unprocessed work: do not ack past it
		}
		ackTo = u.UpdateID
	}
	if ackTo == 0 {
		return nil
	}
	return p.inbox.AdvanceHighWater(ctx, ackTo)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
