package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aegisgo/internal/logx"
)

// Inbox statuses.
const (
	InboxPending = "pending"
	InboxDone    = "done"
	InboxFailed  = "failed"
	InboxSkipped = "skipped" // allowlist denials and non-text updates
)

// highWaterKey is the kv_state key holding the last processed update_id.
const highWaterKey = "telegram.high_water"

// Inbox is the durable at-least-once substrate shared by both transports.
// Webhook and poll handlers only Enqueue; workers lease, process, and mark.
// The long-poll offset advances only past rows marked done — process-then-
// ack, so a crash replays (rather than destroys) unacked work.
type Inbox struct {
	store db
}

// db is the store surface the inbox needs (*store.Store satisfies it).
type db interface {
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) *sql.Row
	Exec(ctx context.Context, query string, args ...any) error
	ExecResult(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NewInbox binds an inbox to the durable store.
func NewInbox(store db) *Inbox { return &Inbox{store: store} }

// Enqueue records an update idempotently (INSERT OR IGNORE on update_id).
// Returns true when the row was newly inserted; false means it was already
// known (webhook redelivery or post-crash replay) — the dedupe answer.
func (i *Inbox) Enqueue(ctx context.Context, u Update) (bool, error) {
	text := ""
	if u.Message != nil {
		text = u.Message.Text
	}
	res, err := i.store.ExecResult(ctx,
		`INSERT OR IGNORE INTO telegram_inbox (update_id, chat_id, text, status, received_ts)
		 VALUES (?,?,?,?,?)`,
		u.UpdateID, chatIDOf(u), text, InboxPending,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// InboxRow is a leased work item.
type InboxRow struct {
	UpdateID int64
	ChatID   int64
	Text     string
	Status   string
	Replied  bool
}

// Pending lists up to n pending rows oldest-first. Simple SELECT (no lease
// column): workers are pooled in-process and claim rows in memory; the DB
// stays the crash-recovery record, not the lock manager.
func (i *Inbox) Pending(ctx context.Context, n int) ([]InboxRow, error) {
	rows, err := i.store.Query(ctx,
		`SELECT update_id, chat_id, text, status FROM telegram_inbox
		 WHERE status=? ORDER BY update_id LIMIT ?`, InboxPending, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxRow
	for rows.Next() {
		var r InboxRow
		if err := rows.Scan(&r.UpdateID, &r.ChatID, &r.Text, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimReply atomically takes ownership of an update's reply: exactly one
// caller across redeliveries, workers, or crashes gets true. The claim is
// set BEFORE the send (claim-then-send): a crash after the claim loses that
// one reply instead of duplicating it — the narrow edge consciously chosen,
// because a duplicate reply is worse UX than a rare lost one and the sender
// can always re-ask.
func (i *Inbox) ClaimReply(ctx context.Context, updateID int64) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := i.store.ExecResult(ctx,
		`UPDATE telegram_inbox SET status=?, replied_at=?, processed_ts=?
		 WHERE update_id=? AND replied_at IS NULL`,
		InboxDone, now, now, updateID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkStatus sets a terminal status without a reply (denials, failures).
func (i *Inbox) MarkStatus(ctx context.Context, updateID int64, status string) error {
	return i.store.Exec(ctx,
		`UPDATE telegram_inbox SET status=?, processed_ts=? WHERE update_id=?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), updateID)
}

// FailAndReopen marks a claimed row failed and clears its claim in one
// write, so a redelivery can retry — claim-then-send's rollback for
// deterministic send failures (API error, rate limit). Never called for
// crash windows: there the durable claim is exactly what prevents
// duplicate replies.
func (i *Inbox) FailAndReopen(ctx context.Context, updateID int64) error {
	return i.store.Exec(ctx,
		`UPDATE telegram_inbox SET status=?, processed_ts=?, replied_at=NULL WHERE update_id=?`,
		InboxFailed, time.Now().UTC().Format(time.RFC3339Nano), updateID)
}

// HighWater returns the last update_id the poll transport may acknowledge
// past (0 = none yet).
func (i *Inbox) HighWater(ctx context.Context) (int64, error) {
	var v string
	err := i.store.QueryRow(ctx, `SELECT v FROM kv_state WHERE k=?`, highWaterKey).Scan(&v)
	if err != nil {
		return 0, nil // absent key = nothing acknowledged yet
	}
	var id int64
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		return 0, fmt.Errorf("parsing high water %q: %w", v, err)
	}
	return id, nil
}

// AdvanceHighWater moves the ack cursor. The poll loop calls this only for
// update_ids at or below the highest fully processed one — see poll.go.
func (i *Inbox) AdvanceHighWater(ctx context.Context, updateID int64) error {
	return i.store.Exec(ctx,
		`INSERT INTO kv_state (k, v) VALUES (?, ?)
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v`, highWaterKey, fmt.Sprintf("%d", updateID))
}

// WorkerPool drains the inbox through the dispatcher with bounded
// concurrency.
type WorkerPool struct {
	inbox    *Inbox
	process  func(ctx context.Context, row InboxRow)
	workers  int
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	stopped bool
	wake    chan struct{}
	done    sync.WaitGroup
}

// NewWorkerPool builds a pool; interval is the idle poll cadence (workers
// also wake immediately on Enqueue via Wake).
func NewWorkerPool(inbox *Inbox, workers int, interval time.Duration,
	process func(ctx context.Context, row InboxRow), logger *slog.Logger) *WorkerPool {
	logger = logx.Or(logger)
	return &WorkerPool{
		inbox: inbox, process: process, workers: workers,
		interval: interval, logger: logger, wake: make(chan struct{}, 1),
	}
}

// Wake nudges idle workers (called after Enqueue for low latency).
func (p *WorkerPool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Start launches the workers under ctx; Stop waits for them to exit.
func (p *WorkerPool) Start(ctx context.Context) {
	for w := 0; w < p.workers; w++ {
		p.done.Add(1)
		go p.worker(ctx)
	}
}

// Stop halts the pool and waits for in-flight rows to finish.
func (p *WorkerPool) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.Wake()
	p.done.Wait()
}

func (p *WorkerPool) worker(ctx context.Context) {
	defer p.done.Done()
	for {
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if stopped {
			return
		}

		rows, err := p.inbox.Pending(ctx, 10)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("telegram inbox read failed", "error", err)
			if !p.sleep(ctx) {
				return
			}
			continue
		}
		if len(rows) == 0 {
			if !p.sleep(ctx) {
				return
			}
			continue
		}
		for _, row := range rows {
			p.mu.Lock()
			stopped := p.stopped
			p.mu.Unlock()
			if stopped {
				return
			}
			p.process(ctx, row)
		}
	}
}

// sleep waits for a wake signal, the tick, or shutdown; false = ctx done.
func (p *WorkerPool) sleep(ctx context.Context) bool {
	t := time.NewTimer(p.interval)
	defer t.Stop()
	select {
	case <-p.wake:
		return true
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func chatIDOf(u Update) int64 {
	if u.Message != nil {
		return u.Message.ChatID()
	}
	return 0
}
