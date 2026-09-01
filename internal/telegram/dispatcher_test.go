package telegram

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/store"
)

// dispatcherWithStore builds a dispatcher over a store the test owns.
// harness hides the store; these cases need to kill it mid-flight to reach
// the claim/mark error arms.
func dispatcherWithStore(t *testing.T, e engineRunner, c Client, logger *slog.Logger) (*Dispatcher, *Inbox, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) // Store.Close is idempotent
	inbox := NewInbox(st)
	return NewDispatcher(e, c, inbox, []int64{chatOK}, nil, logger), inbox, st
}

// TestWaitChatContextCancelled drains the fixed burst (bucketBurst tokens)
// then cancels: the next wait must return the ctx error promptly instead of
// parking for a refill, and throttledSend must not touch the API with a
// dead context.
func TestWaitChatContextCancelled(t *testing.T) {
	c := &fakeClient{}
	d, _ := harness(t, fakeEngine{answer: "x"}, c)
	ctx, cancel := context.WithCancel(context.Background())

	for i := 0; i < int(bucketBurst); i++ {
		if err := d.waitChat(ctx, chatOK); err != nil {
			t.Fatalf("draining burst call %d: %v", i, err)
		}
	}
	cancel()

	start := time.Now()
	if err := d.waitChat(ctx, chatOK); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitChat after cancel = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > bucketMaxWait {
		t.Errorf("waitChat took %v after cancel, want immediate return", elapsed)
	}
	if _, err := d.throttledSend(ctx, chatOK, "hi"); !errors.Is(err, context.Canceled) {
		t.Errorf("throttledSend err = %v, want context.Canceled", err)
	}
	if c.sendCount() != 0 {
		t.Error("SendMessage issued despite cancelled context")
	}
}

// TestWaitChatRefillWaits: a partially-spent bucket waits out only the
// fractional refill time and then delivers — the timer arm of the select.
func TestWaitChatRefillWaits(t *testing.T) {
	c := &fakeClient{}
	d, _ := harness(t, fakeEngine{answer: "x"}, c)

	// Pre-age the bucket to 0.9 tokens so the owed refill is ~100ms
	// (draining the whole burst would owe a full second for the same arm).
	d.mu.Lock()
	d.buckets[chatOK] = &chatBucket{tokens: 0.9, last: time.Now()}
	d.mu.Unlock()

	start := time.Now()
	if err := d.waitChat(context.Background(), chatOK); err != nil {
		t.Fatalf("waitChat after partial drain: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("waitChat returned after %v, want a refill wait", elapsed)
	}
}

// The waitChat drop arm ("chat %d rate limited; dropping", taken when
// now+need exceeds the budget) is deliberately left uncovered: need is
// capped at one second (need = (1-tokens)/bucketRefill with tokens >= 0 and
// a 1 token/s refill) while the budget is bucketMaxWait = 5s, so a lone
// waiter can never oversubscribe — after one bounded wait the refill always
// yields a token. Reaching the drop requires other consumers to hold the
// chat's tokens hostage for the full five-second window; manufacturing that
// from a test means winning a continuous mutex race for five seconds, which
// would be flake, not coverage. The arm exists for genuinely saturated
// chats and shares its statements with the tested ctx-cancel return.

// TestSecondsConversion covers the fractional-seconds conversion the bucket
// refill math depends on.
func TestSecondsConversion(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Duration
	}{
		{0, 0},
		{0.25, 250 * time.Millisecond},
		{1, time.Second},
		{2.5, 2500 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := seconds(tc.in); got != tc.want {
			t.Errorf("seconds(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestProcessClaimErrorLeavesRow: with the store dead between lease and
// claim, Process only logs — the unknown-chat denial cannot be marked (and
// says so), and the known-chat claim failure leaves the row untouched for a
// post-crash replay instead of skipping or sending blind.
func TestProcessClaimErrorLeavesRow(t *testing.T) {
	h := &recHandler{}
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c, slog.New(h))
	ctx := context.Background()

	if _, err := inbox.Enqueue(ctx, Update{UpdateID: 26, Message: &Message{
		Chat: Chat{ID: 999}, Text: "/uptime"}}); err != nil {
		t.Fatal(err)
	}
	unknown, err := inbox.Pending(ctx, 1)
	if err != nil || len(unknown) != 1 {
		t.Fatalf("pending: %v (%v)", unknown, err)
	}
	row := feed(t, inbox, 21, "/uptime")
	st.Close()

	d.Process(ctx, unknown[0])
	if !h.saw("telegram: marking denial") {
		t.Error("denial marking failure not logged")
	}

	d.Process(ctx, row)
	if c.sendCount() != 0 {
		t.Error("reply sent while claim failed")
	}
	if !h.saw("telegram: claiming reply") {
		t.Error("claim failure not logged")
	}
}

// TestClaimAndSendClaimError: the /help path hits the same claim guard —
// a dead store is logged, never answered around.
func TestClaimAndSendClaimError(t *testing.T) {
	h := &recHandler{}
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c, slog.New(h))

	row := feed(t, inbox, 25, "/rules")
	st.Close()
	d.Process(context.Background(), row)

	if c.sendCount() != 0 {
		t.Error("reply sent despite claim failure")
	}
	if !h.saw("telegram: claiming reply") {
		t.Error("claim failure not logged")
	}
}

// TestReleaseOnDeadStore: release's own failures are logged, not propagated
// — there is no caller left to handle them.
func TestReleaseOnDeadStore(t *testing.T) {
	h := &recHandler{}
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c, slog.New(h))

	row := feed(t, inbox, 22, "/uptime")
	if _, err := inbox.ClaimReply(context.Background(), row.UpdateID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	d.release(context.Background(), row.UpdateID)

	if !h.saw("telegram: releasing claim") || !h.saw("telegram: unclaiming") {
		t.Errorf("release failures not logged: releasing=%v unclaiming=%v",
			h.saw("telegram: releasing claim"), h.saw("telegram: unclaiming"))
	}
}

// TestClaimAndSendSkipAndRelease covers the /help path's claim discipline:
// an already-claimed update is never re-sent, and a failed send releases
// the claim so a redelivery can retry.
func TestClaimAndSendSkipAndRelease(t *testing.T) {
	ctx := context.Background()

	// Already claimed elsewhere (post-crash replay): no send.
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	row := feed(t, inbox, 23, "/help")
	if _, err := inbox.ClaimReply(ctx, row.UpdateID); err != nil {
		t.Fatal(err)
	}
	d.Process(ctx, row)
	if c.sendCount() != 0 {
		t.Fatalf("sends = %d, want 0 for an already-claimed update", c.sendCount())
	}

	// Fresh update, failing send: the claim is released for a retry.
	c2 := &fakeClient{sendErr: errFake}
	d2, inbox2 := harness(t, fakeEngine{answer: "x"}, c2)
	d2.Process(ctx, feed(t, inbox2, 24, "/start"))
	if c2.sendCount() != 0 {
		t.Fatal("send unexpectedly succeeded")
	}
	var status string
	if err := stx(t, inbox2, `SELECT status FROM telegram_inbox WHERE update_id=24`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxFailed {
		t.Errorf("status = %q, want failed (claim released for retry)", status)
	}
}

// TestProcessBlankTextSkipped: whitespace-only prompts are skipped without
// touching the engine or the API.
func TestProcessBlankTextSkipped(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)

	d.Process(context.Background(), feed(t, inbox, 32, "   \t "))

	if c.sendCount() != 0 {
		t.Error("blank prompt was answered")
	}
	var status string
	if err := stx(t, inbox, `SELECT status FROM telegram_inbox WHERE update_id=32`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxSkipped {
		t.Errorf("status = %q, want skipped", status)
	}
}

// TestRulesTextVariants: a populated listing, a nil rules func, and an empty
// listing each render distinctly.
func TestRulesTextVariants(t *testing.T) {
	ctx := context.Background()

	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c) // harness rules: one entry
	d.Process(ctx, feed(t, inbox, 30, "/rules"))
	if len(c.sends) != 1 || !strings.Contains(c.sends[0], "Active rules") {
		t.Fatalf("/rules reply = %v, want the active listing", c.sends)
	}

	for _, tc := range []struct {
		name  string
		rules func() []string
		want  string
	}{
		{"nil rules func", nil, "No rules listing available."},
		{"empty rules", func() []string { return nil }, "No rules active."},
	} {
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c2 := &fakeClient{}
		in2 := NewInbox(st)
		d2 := NewDispatcher(fakeEngine{answer: "x"}, c2, in2, []int64{chatOK}, tc.rules, nil)
		d2.Process(ctx, feed(t, in2, 31, "/rules"))
		if len(c2.sends) != 1 || !strings.Contains(c2.sends[0], tc.want) {
			t.Errorf("%s: /rules reply = %v, want %q", tc.name, c2.sends, tc.want)
		}
	}
}

// TestPlaceholderSendFailureReleasesClaim: when the placeholder itself
// cannot be sent (API down), the claim is released so a redelivery retries —
// the answer is never marked done without a visible ack.
func TestPlaceholderSendFailureReleasesClaim(t *testing.T) {
	c := &fakeClient{sendErr: errFake}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)

	d.Process(context.Background(), feed(t, inbox, 33, "tell me a story"))

	if len(c.edits) != 0 {
		t.Errorf("edits = %v, want none (placeholder never landed)", c.edits)
	}
	var status string
	if err := stx(t, inbox, `SELECT status FROM telegram_inbox WHERE update_id=33`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxFailed {
		t.Errorf("status = %q, want failed (claim released for retry)", status)
	}
}

// TestEditAndFallbackBothFail drives the double-failure arm with the real
// HTTPClient against a scripted Bot API: the placeholder lands, the edit
// fails, and the fallback send fails too. The claim already stands, so the
// row stays done — a redelivery must not double-send.
func TestEditAndFallbackBothFail(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	// sendMessage queue: placeholder succeeds, fallback fails.
	api.respond("sendMessage", 0, `{"ok":true,"result":{"message_id":90}}`)
	api.respond("sendMessage", http.StatusInternalServerError, `{"ok":false,"description":"chat not found"}`)
	api.respond("editMessageText", http.StatusInternalServerError, `{"ok":false,"description":"message not modified"}`)

	d, inbox := harness(t, fakeEngine{answer: "answer body"}, c)
	d.Process(context.Background(), feed(t, inbox, 34, "tell me a story"))

	if calls := api.recordedCalls("sendMessage"); len(calls) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (placeholder + failed fallback)", len(calls))
	}
	var status string
	if err := stx(t, inbox, `SELECT status FROM telegram_inbox WHERE update_id=34`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxDone {
		t.Errorf("status = %q, want done (claim stands after double failure)", status)
	}
}

// TestFormatReplyNoOutput: an empty engine answer still acks visibly.
func TestFormatReplyNoOutput(t *testing.T) {
	if got := formatReply(engine.Result{}); got != "(no output)" {
		t.Errorf("formatReply(empty) = %q, want (no output)", got)
	}
}
