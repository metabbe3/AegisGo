package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// fakeEditClient records EditMessageText calls.
type fakeEditClient struct {
	mu    sync.Mutex
	edits []string
}

func (f *fakeEditClient) SendMessage(context.Context, int64, string, int64) (int64, error) {
	return 1, nil
}
func (f *fakeEditClient) SendMessageWithButtons(_ context.Context, _ int64, text string, _ [][]Button) (int64, error) {
	return 42, nil
}
func (f *fakeEditClient) AnswerCallbackQuery(context.Context, string, string) error { return nil }
func (f *fakeEditClient) GetMe(context.Context) (string, error)                     { return "bot", nil }
func (f *fakeEditClient) GetUpdates(context.Context, int64, time.Duration) ([]Update, error) {
	return nil, nil
}
func (f *fakeEditClient) SetWebhook(context.Context, string, string) error { return nil }
func (f *fakeEditClient) DeleteWebhook(context.Context) error              { return nil }
func (f *fakeEditClient) SendMessageButtonsStub()                          {}
func (f *fakeEditClient) EditMessageText(_ context.Context, _, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, text)
	return nil
}
func (f *fakeEditClient) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.edits...)
}

// Registry records → decision enqueues → editor edits the exact message.
func TestEditorEditsDecidedMessage(t *testing.T) {
	fc := &fakeEditClient{}
	reg := NewEditRegistry()
	ed := newEditor(fc, reg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ed.run(ctx)

	reg.Record(3, 100, 42) // approval 3 pushed to chat 100, msg 42
	ed.EnqueueDecided(3, "approved", "telegram:100")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if txts := fc.texts(); len(txts) == 1 {
			want := "✅ Approval #3 approved by Telegram"
			if len(txts[0]) < len(want) || txts[0][:len(want)] != want {
				t.Fatalf("edit text = %q", txts[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("editor never edited the message")
}

// No recorded target → enqueue is a no-op, never a panic (denied via REST
// path where this process never pushed a message).
func TestEditorNoTargetNoop(t *testing.T) {
	fc := &fakeEditClient{}
	ed := newEditor(fc, NewEditRegistry(), nil)
	ed.EnqueueDecided(99, "denied", "rest")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ed.run(ctx)
	time.Sleep(50 * time.Millisecond)
	if n := len(fc.texts()); n != 0 {
		t.Fatalf("edits = %d, want 0", n)
	}
}

// Registry returns a COPY — callers can't race the map.
func TestRegistryTargetsCopy(t *testing.T) {
	reg := NewEditRegistry()
	reg.Record(1, 10, 11)
	ts := reg.Targets(1)
	ts[0].chatID = 999
	if got := reg.Targets(1)[0].chatID; got != 10 {
		t.Fatalf("registry mutated via copy: %d", got)
	}
}

// End-to-end store: notifier records, decide via store, hook fires — the
// wiring contract the app depends on.
func TestNotifierRecordsTargetsForEdit(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fc := &fakeEditClient{}
	reg := NewEditRegistry()
	n := NewNotifier(storeSource{st}, fc, []int64{100}, 10*time.Millisecond, nil)
	n.SetEditRegistry(reg)

	// Prime deterministically, then drive ONE tick manually — no Start
	// goroutine racing its own second prime against our create.
	n.PrimeForTest(context.Background())
	ctx := context.Background()
	id, err := st.CreateApproval(ctx, "k", "{}", "r")
	if err != nil {
		t.Fatal(err)
	}
	n.TickForTest(ctx)
	ts := reg.Targets(id)
	if len(ts) != 1 || ts[0].messageID != 42 {
		t.Fatalf("targets = %+v (chat 100 msg 42 wanted)", ts)
	}
}

// storeSource adapts *store.Store to ApprovalSource for tests.
type storeSource struct{ st *store.Store }

func (s storeSource) PendingApprovals(ctx context.Context, limit int) ([]ApprovalInfo, error) {
	pend, err := s.st.PendingApprovals(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalInfo, len(pend))
	for i, a := range pend {
		out[i] = ApprovalInfo{ID: a.ID, Kind: a.Kind, Payload: a.Payload, Reason: a.Reason}
	}
	return out, nil
}
