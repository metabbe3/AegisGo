package telegram

import (
	"context"
	"strings"
	"testing"

	"aegisgo/internal/store"
)

// fakeSink embeds the shared fakeClient (full Client interface) and
// records digest sends.
type fakeSink struct {
	*fakeClient
	chats []int64
	texts []string
}

func (f *fakeSink) SendMessage(ctx context.Context, chat int64, text string, replyTo int64) (int64, error) {
	f.chats = append(f.chats, chat)
	f.texts = append(f.texts, text)
	return 0, nil
}

// The digest is one human message: uptime, runs, pending, decisions.
func TestDigestHuman(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	id, _ := st.CreateApproval(ctx, "system_command", `{"command":"uptime"}`, "L2")
	st.DecideApproval(ctx, id, "approved", "telegram:100")

	sink := &fakeSink{fakeClient: &fakeClient{}}
	dg := NewDigest(sink, []int64{42}, st, st, st, nil)
	dg.Send(ctx)

	if len(sink.texts) != 1 {
		t.Fatalf("digest sends = %d, want 1", len(sink.texts))
	}
	got := sink.texts[0]
	for _, want := range []string{"uptime", "runs", "no approvals waiting", "✅ #1 System command"} {
		if !strings.Contains(got, want) {
			t.Fatalf("digest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "_") && strings.Contains(got, "regex_router") {
		t.Fatalf("machine tokens leaked: %q", got)
	}
}

// A decided-denied row renders the deny icon.
func TestDigestDeniedIcon(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	id, _ := st.CreateApproval(ctx, "k", `{}`, "r")
	st.DecideApproval(ctx, id, "denied", "cli")

	sink := &fakeSink{fakeClient: &fakeClient{}}
	dg := NewDigest(sink, []int64{42}, st, st, st, nil)
	dg.Send(ctx)
	if !strings.Contains(sink.texts[0], "🚫 #1") {
		t.Fatalf("want deny icon: %q", sink.texts[0])
	}
}
