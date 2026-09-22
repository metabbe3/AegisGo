package telegram

import (
	"context"
	"errors"
	"io"
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
	return NewDispatcher(e, c, inbox, []int64{chatOK}, nil, logger, nil), inbox, st
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

	if !h.saw("telegram: releasing claim") {
		t.Error("release failure not logged")
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
		d2 := NewDispatcher(fakeEngine{answer: "x"}, c2, in2, []int64{chatOK}, tc.rules, nil, nil)
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

// --- HITL commands (ADR-0003 wiring) ---

// fakeApprover stands in for *store.Store when tests only exercise text.
type fakeApprover struct {
	pending []store.Approval
	decided map[int64]string
	fail    bool
}

func (f *fakeApprover) PendingApprovals(ctx context.Context, limit int) ([]store.Approval, error) {
	if f.fail {
		return nil, errFake
	}
	return f.pending, nil
}

func (f *fakeApprover) DecideApproval(ctx context.Context, id int64, state, by string) (bool, error) {
	if f.fail {
		return false, errFake
	}
	for _, a := range f.pending {
		if a.ID == id {
			if _, ok := f.decided[id]; ok {
				return false, nil
			}
			if f.decided == nil {
				f.decided = map[int64]string{}
			}
			f.decided[id] = state
			return true, nil
		}
	}
	return false, nil
}

// lastSend returns the most recent sent text (test helper).
func lastSend(c *fakeClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sends) == 0 {
		return ""
	}
	return c.sends[len(c.sends)-1]
}

func newHitlDispatcher(t *testing.T, ap approver) (*Dispatcher, *Inbox, *fakeClient) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	c := &fakeClient{}
	inbox := NewInbox(st)
	rules := func() []string {
		return []string{"uptime → /uptime → system_command"}
	}
	return NewDispatcher(fakeEngine{answer: "x"}, c, inbox, []int64{chatOK}, rules, logger, ap), inbox, c
}

func TestApprovalsCommandLists(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{
		{ID: 1, Kind: "system_command", Payload: `{"command":"restart"}`, Reason: "L2: restart"},
	}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feed(t, inbox, 9001, "/approvals"))
	if !strings.Contains(lastSend(c), "#1 · System command") || !strings.Contains(lastSend(c), "Command restart") || !strings.Contains(lastSend(c), "/approve 1") {
		t.Errorf("approvals text wrong: %q", lastSend(c))
	}
}

func TestApprovalsEmpty(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.Process(t.Context(), feed(t, inbox, 9002, "/approvals"))
	if !strings.Contains(lastSend(c), "No pending") {
		t.Errorf("empty text wrong: %q", lastSend(c))
	}
}

func TestApproveWithID(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{{ID: 3, Kind: "k", Payload: "{}", Reason: "r"}}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feed(t, inbox, 9003, "/approve 3"))
	if !strings.Contains(lastSend(c), "#3 approved") {
		t.Errorf("approve text wrong: %q", lastSend(c))
	}
	if ap.decided[3] != "approved" {
		t.Errorf("decided = %v, want approved", ap.decided)
	}
}

func TestDenyWithID(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{{ID: 4, Kind: "k", Payload: "{}", Reason: "r"}}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feed(t, inbox, 9004, "/deny 4"))
	if !strings.Contains(lastSend(c), "#4 denied") {
		t.Errorf("deny text wrong: %q", lastSend(c))
	}
}

func TestApproveBareUsesOldest(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{
		{ID: 7, Kind: "k", Payload: "{}", Reason: "r"},
		{ID: 9, Kind: "k", Payload: "{}", Reason: "r"},
	}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feed(t, inbox, 9005, "/approve"))
	if !strings.Contains(lastSend(c), "#7 approved") {
		t.Errorf("bare approve should pick oldest: %q", lastSend(c))
	}
}

func TestApproveNoopSurfacesHonestly(t *testing.T) {
	ap := &fakeApprover{} // nothing pending
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feed(t, inbox, 9006, "/approve 12"))
	if !strings.Contains(lastSend(c), "not pending") && !strings.Contains(lastSend(c), "Nothing pending") {
		t.Errorf("no-op must be surfaced, got: %q", lastSend(c))
	}
}

func TestApproveInvalidIDRejected(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.Process(t.Context(), feed(t, inbox, 9007, "/approve abc"))
	if !strings.Contains(lastSend(c), "Not a valid approval id") {
		t.Errorf("invalid id text wrong: %q", lastSend(c))
	}
}

func TestApprovalsUnwired(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, nil)
	d.Process(t.Context(), feed(t, inbox, 9008, "/approvals"))
	if !strings.Contains(lastSend(c), "unavailable") {
		t.Errorf("unwired text wrong: %q", lastSend(c))
	}
}

func TestUnknownSlashCommandNeverHitsEngine(t *testing.T) {
	// Owner rule: command replies must be AI-free. An unknown slash-command
	// must be answered deterministically and must NOT reach the engine.
	d, inbox, c := newHitlDispatcher(t, nil)
	d.Process(t.Context(), feed(t, inbox, 9010, "/definitely_not_a_command"))
	body := lastSend(c)
	if !strings.Contains(body, "Unknown command") || !strings.Contains(body, "/uptime") {
		t.Errorf("unknown-command reply wrong: %q", body)
	}
}

// feedCallback inserts a button-press update and returns its inbox row.
func feedCallback(t *testing.T, inbox *Inbox, updateID int64, data string) InboxRow {
	t.Helper()
	u := Update{UpdateID: updateID,
		Callback: &CallbackQuery{ID: "cbq", Data: data,
			Message: &Message{MessageID: updateID * 10, Chat: Chat{ID: chatOK}}}}
	if _, err := inbox.Enqueue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	rows, err := inbox.Pending(context.Background(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending: %v (%d)", err, len(rows))
	}
	return rows[0]
}

func TestCallbackApproveButton(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{{ID: 6, Kind: "k", Payload: "{}", Reason: "r"}}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feedCallback(t, inbox, 9100, "apr:6"))
	if !strings.Contains(lastSend(c), "#6 approved") {
		t.Errorf("callback approve reply wrong: %q", lastSend(c))
	}
	if ap.decided[6] != "approved" {
		t.Errorf("decided = %v, want approved", ap.decided)
	}
}

func TestCallbackDenyButton(t *testing.T) {
	ap := &fakeApprover{pending: []store.Approval{{ID: 7, Kind: "k", Payload: "{}", Reason: "r"}}}
	d, inbox, c := newHitlDispatcher(t, ap)
	d.Process(t.Context(), feedCallback(t, inbox, 9101, "dny:7"))
	if !strings.Contains(lastSend(c), "#7 denied") {
		t.Errorf("callback deny reply wrong: %q", lastSend(c))
	}
}

func TestCallbackBadData(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.Process(t.Context(), feedCallback(t, inbox, 9102, "junk"))
	if !strings.Contains(lastSend(c), "Bad callback data") {
		t.Errorf("bad data reply wrong: %q", lastSend(c))
	}
}

// /status happy path: uptime line + stats from the store snapshot.
func TestStatusCommand(t *testing.T) {
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetStats(st)
	d.Process(t.Context(), feed(t, inbox, 9103, "/status"))
	got := lastSend(c)
	if !strings.Contains(got, "uptime") || !strings.Contains(got, "runs") {
		t.Fatalf("/status reply = %q", got)
	}
	if !strings.Contains(got, "🩺") {
		t.Fatalf("missing status header: %q", got)
	}
}

// /status without stats wired degrades honestly.
func TestStatusNoStats(t *testing.T) {
	d, inbox, c := newHitlDispatcher(t, &fakeApprover{})
	d.SetStats(nil)
	d.Process(t.Context(), feed(t, inbox, 9104, "/status"))
	got := lastSend(c)
	if !strings.Contains(got, "stats unavailable") {
		t.Fatalf("want stats unavailable, got %q", got)
	}
}

// /history lists decided rows newest-first with human verdict icons.
func TestHistoryCommand(t *testing.T) {
	c := &fakeClient{}
	d, inbox, st := dispatcherWithStore(t, fakeEngine{answer: "x"}, c,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := t.Context()
	id1, _ := st.CreateApproval(ctx, "system_command", `{"command":"restart"}`, "L2")
	st.DecideApproval(ctx, id1, "approved", "telegram:100")
	id2, _ := st.CreateApproval(ctx, "system_command", `{"command":"reload"}`, "L2")
	st.DecideApproval(ctx, id2, "denied", "cli")
	d.SetHistory(st)
	d.Process(ctx, feed(t, inbox, 9105, "/history"))
	got := lastSend(c)
	if !strings.Contains(got, "✅ #") || !strings.Contains(got, "🚫 #") {
		t.Fatalf("history missing verdicts: %q", got)
	}
	if !strings.Contains(got, "System command") {
		t.Fatalf("history not human: %q", got)
	}
}

// /history without the store wired degrades honestly.
func TestHistoryNoStore(t *testing.T) {
	c := &fakeClient{}
	d, inbox, _ := dispatcherWithStore(t, fakeEngine{answer: "x"}, c,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Process(t.Context(), feed(t, inbox, 9106, "/history"))
	if !strings.Contains(lastSend(c), "unavailable") {
		t.Fatalf("want unavailable, got %q", lastSend(c))
	}
}
