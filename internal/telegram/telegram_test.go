package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/store"
)

// fakeClient records every send/edit; configurable failures.
type fakeClient struct {
	mu        sync.Mutex
	sends     []string // message texts, in order
	edits     []string
	nextMsgID int64
	sendErr   error
	editErr   error
	updates   []Update // returned by GetUpdates
	// getUpdatesErr is one-shot: the first GetUpdates call fails with it
	// (a transient API error), later calls return updates normally.
	getUpdatesErr error
}

func (f *fakeClient) GetMe(context.Context) (string, error) { return "aegis_test_bot", nil }

func (f *fakeClient) SendMessage(_ context.Context, _ int64, text string, _ int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sends = append(f.sends, text)
	if f.nextMsgID == 0 {
		f.nextMsgID = 100
	}
	f.nextMsgID++
	return f.nextMsgID, nil
}

func (f *fakeClient) EditMessageText(_ context.Context, _, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, text)
	return nil
}

func (f *fakeClient) GetUpdates(_ context.Context, offset int64, _ time.Duration) ([]Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getUpdatesErr != nil {
		err := f.getUpdatesErr
		f.getUpdatesErr = nil
		return nil, err
	}
	// Mirror Telegram offset semantics: un-acked updates (update_id >=
	// offset) are re-delivered on every call until the poller confirms
	// past them. This is what lets the loop's advance() retry until work
	// is fully processed — a one-shot batch would never be acked.
	var out []Update
	for _, u := range f.updates {
		if u.UpdateID >= offset {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeClient) SetWebhook(context.Context, string, string) error { return nil }
func (f *fakeClient) DeleteWebhook(context.Context) error              { return nil }

func (f *fakeClient) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// fakeEngine: slash-prefixed prompts are router hits, others LLM runs.
type fakeEngine struct {
	answer string
}

func (f fakeEngine) Run(_ context.Context, prompt string) engine.Result {
	if strings.HasPrefix(prompt, "/") {
		return engine.Result{Answer: f.answer, DecisionSource: store.SourceRouter,
			RuleID: "uptime", LatencyMS: 3}
	}
	return engine.Result{Answer: f.answer, DecisionSource: store.SourceLLM, LatencyMS: 900}
}

const chatOK int64 = 424242

// harness wires a dispatcher over an in-memory inbox.
func harness(t *testing.T, e engineRunner, c Client) (*Dispatcher, *Inbox) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	inbox := NewInbox(st)
	d := NewDispatcher(e, c, inbox, []int64{chatOK}, func() []string {
		return []string{"uptime → /uptime → system_command"}
	}, nil)
	return d, inbox
}

func feed(t *testing.T, inbox *Inbox, updateID int64, text string) InboxRow {
	t.Helper()
	u := Update{UpdateID: updateID, Message: &Message{
		MessageID: updateID * 10, Chat: Chat{ID: chatOK}, Text: text}}
	if _, err := inbox.Enqueue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	rows, err := inbox.Pending(context.Background(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending: %v %v", rows, err)
	}
	return rows[0]
}

func TestDispatcherAllowlist(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "hi"}, c)

	// Enqueue from an unknown chat: skipped, never answered.
	u := Update{UpdateID: 1, Message: &Message{Chat: Chat{ID: 999}, Text: "/uptime"}}
	if _, err := inbox.Enqueue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	rows, _ := inbox.Pending(context.Background(), 1)
	d.Process(context.Background(), rows[0])

	if c.sendCount() != 0 {
		t.Error("non-allowlisted chat was answered")
	}
	var status string
	if err := stx(t, inbox, `SELECT status FROM telegram_inbox WHERE update_id=1`, &status); err != nil {
		t.Fatal(err)
	}
	if status != InboxSkipped {
		t.Errorf("status = %q, want skipped", status)
	}
}

func stx(t *testing.T, inbox *Inbox, q string, dest *string) error {
	t.Helper()
	return inbox.store.QueryRow(context.Background(), q).Scan(dest)
}

func TestRouterHitDirectReply(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "up 5 days"}, c)

	d.Process(context.Background(), feed(t, inbox, 2, "/uptime"))

	if c.sendCount() != 1 {
		t.Fatalf("sends = %d, want 1 (no placeholder on router hits)", c.sendCount())
	}
	if len(c.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(c.edits))
	}
	if !strings.Contains(c.sends[0], "[regex_router via uptime") ||
		!strings.Contains(c.sends[0], "up 5 days") {
		t.Errorf("reply = %q", c.sends[0])
	}
}

func TestPlaceholderEditFlow(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "the llm answer"}, c)

	d.Process(context.Background(), feed(t, inbox, 3, "tell me a story"))

	if c.sendCount() != 1 || c.sends[0] != placeholderText {
		t.Errorf("placeholder sends = %v", c.sends)
	}
	if len(c.edits) != 1 || !strings.Contains(c.edits[0], "the llm answer") {
		t.Errorf("edits = %v", c.edits)
	}
}

func TestEditFailureFallsBackToSend(t *testing.T) {
	c := &fakeClient{editErr: errFake}
	d, inbox := harness(t, fakeEngine{answer: "answer body"}, c)

	d.Process(context.Background(), feed(t, inbox, 4, "tell me a story"))

	if c.sendCount() != 2 { // placeholder + fallback
		t.Errorf("sends = %d (%v), want 2", c.sendCount(), c.sends)
	}
	if !strings.Contains(c.sends[1], "answer body") {
		t.Errorf("fallback send = %q", c.sends[1])
	}
}

func TestTruncation(t *testing.T) {
	c := &fakeClient{}
	long := strings.Repeat("x", maxReplyLen+500)
	d, inbox := harness(t, fakeEngine{answer: long}, c)

	d.Process(context.Background(), feed(t, inbox, 5, "/uptime"))

	if !strings.Contains(c.sends[0], "(truncated)") {
		t.Error("truncation notice missing")
	}
	if len(c.sends[0]) > maxReplyLen+100 {
		t.Errorf("reply length = %d", len(c.sends[0]))
	}
}

// TestCrashWindowExactlyOneReply is the mandated crash-simulation: the same
// update_id delivered twice (webhook retry or post-crash replay) must
// produce exactly one reply — claim-then-send holds the line.
func TestCrashWindowExactlyOneReply(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "once"}, c)
	ctx := context.Background()

	row := feed(t, inbox, 6, "/uptime")
	d.Process(ctx, row)
	// Crash simulation: redeliver the identical update and process again.
	d.Process(ctx, row)
	// And once more via a fresh pending read, for good measure.
	d.Process(ctx, row)

	if n := c.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want exactly 1 (duplicate reply leaked)", n)
	}
}

func TestHelpAndRules(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)

	d.Process(context.Background(), feed(t, inbox, 7, "/help"))
	d.Process(context.Background(), feed(t, inbox, 8, "/rules"))

	if c.sendCount() != 2 {
		t.Fatalf("sends = %d, want 2", c.sendCount())
	}
	if !strings.Contains(c.sends[0], "Router commands") {
		t.Errorf("help = %q", c.sends[0])
	}
	if !strings.Contains(c.sends[1], "uptime") {
		t.Errorf("rules = %q", c.sends[1])
	}
}

// TestOffsetOrdering is the mandated ack-cursor test: the high water never
// advances past unprocessed work — a crash must replay, not destroy.
func TestOffsetOrdering(t *testing.T) {
	c := &fakeClient{}
	_, inbox := harness(t, fakeEngine{answer: "x"}, c)
	ctx := context.Background()
	p := NewPollLoop(c, inbox, nil, nil)

	// Updates 1..3 fetched; none processed yet: no advance at all.
	for _, id := range []int64{1, 2, 3} {
		if _, err := inbox.Enqueue(ctx, Update{UpdateID: id, Message: &Message{Chat: Chat{ID: chatOK}, Text: "/uptime"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.advance(ctx, []Update{{UpdateID: 1}, {UpdateID: 2}, {UpdateID: 3}}); err != nil {
		t.Fatal(err)
	}
	if hw, _ := inbox.HighWater(ctx); hw != 0 {
		t.Fatalf("high water = %d with nothing processed, want 0", hw)
	}

	// Process 1 and 2 (claim = done): advance may reach 2, NOT 3.
	for _, id := range []int64{1, 2} {
		if _, err := inbox.ClaimReply(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.advance(ctx, []Update{{UpdateID: 1}, {UpdateID: 2}, {UpdateID: 3}}); err != nil {
		t.Fatal(err)
	}
	hw, _ := inbox.HighWater(ctx)
	if hw != 2 {
		t.Fatalf("high water = %d, want 2 (must stop before pending 3)", hw)
	}

	// 3 processed: full advance.
	if _, err := inbox.ClaimReply(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := p.advance(ctx, []Update{{UpdateID: 1}, {UpdateID: 2}, {UpdateID: 3}}); err != nil {
		t.Fatal(err)
	}
	if hw, _ = inbox.HighWater(ctx); hw != 3 {
		t.Fatalf("high water = %d, want 3", hw)
	}
}

func TestWebhookSecretAndAck(t *testing.T) {
	c := &fakeClient{}
	_, inbox := harness(t, fakeEngine{answer: "x"}, c)
	pool := NewWorkerPool(inbox, 1, time.Hour, func(context.Context, InboxRow) {}, nil)
	h := NewWebhookHandler("s3cret", inbox, pool, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Wrong secret: 401, nothing enqueued.
	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"update_id":10,"message":{"chat":{"id":1},"text":"/uptime"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong secret status = %d", resp.StatusCode)
	}

	// Right secret: instant 200, update enqueued.
	req, _ := http.NewRequest("POST", srv.URL,
		strings.NewReader(`{"update_id":10,"message":{"message_id":7,"chat":{"id":1},"text":"/uptime"}}`))
	req.Header.Set(WebhookHeader, "s3cret")
	start := time.Now()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("webhook ack waited on processing (must be instant)")
	}
	rows, err := inbox.Pending(context.Background(), 1)
	if err != nil || len(rows) != 1 || rows[0].UpdateID != 10 {
		t.Fatalf("inbox = %v, %v", rows, err)
	}

	// Redelivery of the same update_id is deduped at enqueue.
	if known, _ := inbox.Enqueue(context.Background(), Update{
		UpdateID: 10, Message: &Message{MessageID: 7, Chat: Chat{ID: 1}, Text: "/uptime"}}); known {
		t.Error("redelivery reported as new")
	}
}

func TestSendFailureReleasesClaim(t *testing.T) {
	c := &fakeClient{sendErr: errFake}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	ctx := context.Background()

	d.Process(ctx, feed(t, inbox, 11, "/uptime"))
	if c.sendCount() != 0 {
		t.Fatal("expected failed send")
	}

	// The claim was released: a redelivery can retry.
	var repliedAt *string
	if err := inbox.store.QueryRow(ctx,
		`SELECT replied_at FROM telegram_inbox WHERE update_id=11`).Scan(&repliedAt); err != nil {
		t.Fatal(err)
	}
	if repliedAt != nil {
		t.Error("claim not released after deterministic send failure")
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "fake api error" }
