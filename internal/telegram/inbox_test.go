package telegram

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// waitFor polls cond every ~10ms until it holds or the deadline passes, then
// fails the test. Async behavior (workers, poll loops) is observed, never
// assumed via fixed sleeps.
func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", deadline)
}

// recHandler records slog messages so async error branches (worker read
// failures, claim errors) are directly observable. Same pattern as
// internal/router/rulesstore_test.go.
type recHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

func (h *recHandler) saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// openInbox opens a fresh in-memory inbox. The returned store is owned by
// the test (some cases close it early to model a dead database).
func openInbox(t *testing.T) (*Inbox, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) // Store.Close is idempotent
	return NewInbox(st), st
}

// stopSoon fails the test instead of hanging when Stop never returns — Stop
// must always drain its workers.
func stopSoon(t *testing.T, p *WorkerPool) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WorkerPool.Stop did not return")
	}
}

// TestWorkerPoolProcessesRows drives the real pool against a real inbox:
// workers lease pending rows oldest-first, hand each to process, and Stop
// waits for the drain.
func TestWorkerPoolProcessesRows(t *testing.T) {
	inbox, _ := openInbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	seen := make(map[int64]int64) // update_id -> chat_id
	pool := NewWorkerPool(inbox, 2, 10*time.Millisecond, func(ctx context.Context, row InboxRow) {
		mu.Lock()
		seen[row.UpdateID] = row.ChatID
		mu.Unlock()
		if err := inbox.MarkStatus(ctx, row.UpdateID, InboxDone); err != nil {
			t.Errorf("marking %d done: %v", row.UpdateID, err)
		}
	}, nil)

	for _, id := range []int64{1, 2, 3} {
		if _, err := inbox.Enqueue(ctx, Update{UpdateID: id, Message: &Message{
			MessageID: id * 10, Chat: Chat{ID: chatOK}, Text: "/uptime"}}); err != nil {
			t.Fatal(err)
		}
	}
	// A message-less update leases too (chat_id 0) — denying it is the
	// dispatcher's job, not the pool's.
	if _, err := inbox.Enqueue(ctx, Update{UpdateID: 4}); err != nil {
		t.Fatal(err)
	}

	pool.Start(ctx)
	pool.Wake()
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 4
	})
	mu.Lock()
	if seen[4] != 0 {
		t.Errorf("message-less update chat_id = %d, want 0", seen[4])
	}
	mu.Unlock()
	// Every row reached a terminal state: nothing pending remains.
	waitFor(t, 5*time.Second, func() bool {
		rows, err := inbox.Pending(ctx, 10)
		return err == nil && len(rows) == 0
	})
	stopSoon(t, pool)
}

// TestWorkerPoolStopsOnCancel: a cancelled context must release workers even
// mid-sleep, and Stop must still return rather than hang.
func TestWorkerPoolStopsOnCancel(t *testing.T) {
	inbox, _ := openInbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	pool := NewWorkerPool(inbox, 2, time.Hour, func(context.Context, InboxRow) {}, nil)
	pool.Start(ctx)
	cancel()
	stopSoon(t, pool)
}

// TestWorkerInboxReadError: a dead store must not kill the worker (log +
// retry on the next tick) and never invents work to process; cancellation
// ends the retry loop.
func TestWorkerInboxReadError(t *testing.T) {
	inbox, st := openInbox(t)
	st.Close() // every inbox read fails from here

	h := &recHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	pool := NewWorkerPool(inbox, 1, 5*time.Millisecond, func(context.Context, InboxRow) {
		t.Error("process ran despite unreadable inbox")
	}, slog.New(h))
	pool.Start(ctx)
	waitFor(t, 5*time.Second, func() bool { return h.saw("telegram inbox read failed") })
	cancel()
	stopSoon(t, pool)
}

// TestInboxClosedStoreErrors pins the error contract of every inbox
// operation against a closed store: failures are returned to the caller,
// with HighWater's swallow as the one deliberate exception.
func TestInboxClosedStoreErrors(t *testing.T) {
	inbox, st := openInbox(t)
	st.Close()
	ctx := context.Background()

	if _, err := inbox.Enqueue(ctx, Update{UpdateID: 1}); err == nil {
		t.Error("Enqueue on closed store: want error")
	}
	if _, err := inbox.Pending(ctx, 1); err == nil {
		t.Error("Pending on closed store: want error")
	}
	if _, err := inbox.ClaimReply(ctx, 1); err == nil {
		t.Error("ClaimReply on closed store: want error")
	}
	if err := inbox.MarkStatus(ctx, 1, InboxFailed); err == nil {
		t.Error("MarkStatus on closed store: want error")
	}
	if err := inbox.FailAndReopen(ctx, 1); err == nil {
		t.Error("FailAndReopen on closed store: want error")
	}
	if err := inbox.AdvanceHighWater(ctx, 1); err == nil {
		t.Error("AdvanceHighWater on closed store: want error")
	}
	// HighWater deliberately folds read errors into 0 ("nothing acked
	// yet"): refetching from offset 1 is safe (Enqueue dedupes) while a
	// wrongly-advanced cursor would destroy messages — failing open is the
	// safe side.
	if hw, err := inbox.HighWater(ctx); hw != 0 || err != nil {
		t.Errorf("HighWater on closed store = (%d, %v), want (0, nil)", hw, err)
	}
}

// TestHighWaterGarbageValue: a corrupted kv_state value surfaces as a parse
// error instead of silently resetting the ack cursor.
func TestHighWaterGarbageValue(t *testing.T) {
	inbox, _ := openInbox(t)
	ctx := context.Background()
	if err := inbox.store.Exec(ctx,
		`INSERT INTO kv_state (k, v) VALUES (?, 'not-a-number')`, highWaterKey); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.HighWater(ctx); err == nil {
		t.Fatal("HighWater with garbage value: want parse error")
	}
}

// TestWebhookEdgeCases: poison payloads are acked 200 and dropped (anything
// else makes Telegram retry them forever), while an unusable inbox answers
// 500 so the delivery is retried — at-least-once.
func TestWebhookEdgeCases(t *testing.T) {
	inbox, _ := openInbox(t)
	pool := NewWorkerPool(inbox, 1, time.Hour, func(context.Context, InboxRow) {}, nil)
	h := NewWebhookHandler("s3cret", inbox, pool, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(secret, body string) int {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		req.Header.Set(WebhookHeader, secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("s3cret", `{not json`); got != http.StatusOK {
		t.Errorf("garbage payload status = %d, want 200 (drop, never retry-loop)", got)
	}
	if got := post("s3cret", `{"message":{"chat":{"id":1},"text":"hi"}}`); got != http.StatusOK {
		t.Errorf("payload without update_id status = %d, want 200", got)
	}
	if rows, err := inbox.Pending(context.Background(), 10); err != nil || len(rows) != 0 {
		t.Fatalf("inbox = %v (%v), want empty after dropped payloads", rows, err)
	}

	// A valid delivery enqueues; its redelivery is deduped yet still 200 —
	// Telegram retries must not turn into duplicate work.
	valid := `{"update_id":41,"message":{"chat":{"id":1},"text":"/uptime"}}`
	if got := post("s3cret", valid); got != http.StatusOK {
		t.Errorf("valid payload status = %d, want 200", got)
	}
	if got := post("s3cret", valid); got != http.StatusOK {
		t.Errorf("redelivery status = %d, want 200", got)
	}
	if rows, err := inbox.Pending(context.Background(), 10); err != nil || len(rows) != 1 || rows[0].UpdateID != 41 {
		t.Fatalf("inbox = %v (%v), want exactly update 41 after redelivery", rows, err)
	}

	// Dead inbox: 500 so Telegram redelivers.
	dead, st := openInbox(t)
	st.Close()
	hDead := NewWebhookHandler("s3cret", dead, pool, nil)
	srvDead := httptest.NewServer(hDead)
	defer srvDead.Close()
	req, _ := http.NewRequest("POST", srvDead.URL,
		strings.NewReader(`{"update_id":40,"message":{"chat":{"id":1},"text":"/uptime"}}`))
	req.Header.Set(WebhookHeader, "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("enqueue-failure status = %d, want 500", resp.StatusCode)
	}
}
