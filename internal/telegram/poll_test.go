package telegram

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestPollLoopDeliversAndAdvances drives the whole long-poll pipeline with
// real components on both sides of the Client seam: scripted updates flow
// through Run into the durable inbox, the worker pool processes them via the
// dispatcher, and only then does the ack cursor move (process-then-ack).
func TestPollLoopDeliversAndAdvances(t *testing.T) {
	c := &fakeClient{updates: []Update{{UpdateID: 6, Message: &Message{
		MessageID: 60, Chat: Chat{ID: chatOK}, Text: "/uptime"}}}}
	d, inbox := harness(t, fakeEngine{answer: "up 2 days"}, c)
	ctx, cancel := context.WithCancel(context.Background())

	pool := NewWorkerPool(inbox, 2, 10*time.Millisecond, d.Process, nil)
	p := NewPollLoop(c, inbox, pool, nil)
	pool.Start(ctx)
	go p.Run(ctx)

	// The reply went out exactly once (claim idempotency) and the cursor
	// advanced past the update only after processing.
	waitFor(t, 5*time.Second, func() bool {
		if c.sendCount() != 1 {
			return false
		}
		hw, err := inbox.HighWater(ctx)
		return err == nil && hw == 6
	})
	var status string
	if err := stx(t, inbox, `SELECT status FROM telegram_inbox WHERE update_id=6`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxDone {
		t.Errorf("row status = %q, want done", status)
	}

	cancel()
	stopSoon(t, pool)
}

// TestPollLoopGetUpdatesErrorBackoff: one failed poll backs off (pollMinBack
// is a fixed second, making this the package's slowest test, ~1s) and the
// loop then recovers — it is the only ingress for NAT'd boxes, so a
// transient API error can never be fatal.
func TestPollLoopGetUpdatesErrorBackoff(t *testing.T) {
	c := &fakeClient{
		getUpdatesErr: errFake, // first poll fails, the update flows after
		updates: []Update{{UpdateID: 7, Message: &Message{
			MessageID: 70, Chat: Chat{ID: chatOK}, Text: "/uptime"}}},
	}
	d, inbox := harness(t, fakeEngine{answer: "ok"}, c)
	h := &recHandler{}
	ctx, cancel := context.WithCancel(context.Background())

	pool := NewWorkerPool(inbox, 1, 10*time.Millisecond, d.Process, slog.New(h))
	p := NewPollLoop(c, inbox, pool, slog.New(h))
	pool.Start(ctx)
	go p.Run(ctx)

	waitFor(t, 5*time.Second, func() bool {
		return h.saw("telegram: getUpdates failed; backing off")
	})
	// Recovery: despite the earlier failure the update is delivered and
	// acked exactly once.
	waitFor(t, 5*time.Second, func() bool {
		hw, err := inbox.HighWater(ctx)
		return err == nil && hw == 7 && c.sendCount() == 1
	})

	cancel()
	stopSoon(t, pool)
}

// TestPollLoopHighWaterReadError: a corrupted high-water value must not kill
// the transport (log + back off and retry), and cancellation must end it.
func TestPollLoopHighWaterReadError(t *testing.T) {
	inbox, _ := openInbox(t)
	if err := inbox.store.Exec(context.Background(),
		`INSERT INTO kv_state (k, v) VALUES (?, 'garbage')`, highWaterKey); err != nil {
		t.Fatal(err)
	}
	h := &recHandler{}
	// Run calls pool.Wake(), so the pool must exist — starting it is not
	// needed for the cursor path under test.
	pool := NewWorkerPool(inbox, 1, time.Hour, func(context.Context, InboxRow) {}, nil)
	p := NewPollLoop(&fakeClient{}, inbox, pool, slog.New(h))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	waitFor(t, 5*time.Second, func() bool { return h.saw("telegram: reading high water") })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestAdvanceGapBlocks: the ack cursor stops before any still-pending row,
// and a non-positive update_id (never ackable) must not unblock it either —
// a gap can never let a later update drag the offset past unprocessed work.
func TestAdvanceGapBlocks(t *testing.T) {
	c := &fakeClient{}
	_, inbox := harness(t, fakeEngine{answer: "x"}, c)
	ctx := context.Background()

	for _, id := range []int64{1, 2} {
		if _, err := inbox.Enqueue(ctx, Update{UpdateID: id,
			Message: &Message{Chat: Chat{ID: chatOK}, Text: "/uptime"}}); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPollLoop(c, inbox, nil, nil)

	// Nothing processed: no advance at all.
	if err := p.advance(ctx, []Update{{UpdateID: 0}, {UpdateID: 1}, {UpdateID: 2}}); err != nil {
		t.Fatal(err)
	}
	if hw, _ := inbox.HighWater(ctx); hw != 0 {
		t.Fatalf("high water = %d with all rows pending, want 0", hw)
	}

	// Row 1 done, row 2 still pending: the cursor stops at 1.
	if _, err := inbox.ClaimReply(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.advance(ctx, []Update{{UpdateID: 0}, {UpdateID: 1}, {UpdateID: 2}}); err != nil {
		t.Fatal(err)
	}
	if hw, _ := inbox.HighWater(ctx); hw != 1 {
		t.Fatalf("high water = %d, want 1 (pending row 2 blocks the gap)", hw)
	}
}

func TestSleepCtx(t *testing.T) {
	// Timer arm: the duration elapses and reports true.
	if !sleepCtx(context.Background(), 5*time.Millisecond) {
		t.Error("sleepCtx with live ctx = false, want true")
	}
	// Ctx arm: an already-done context wins over any duration.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx with done ctx = true, want false")
	}
}
