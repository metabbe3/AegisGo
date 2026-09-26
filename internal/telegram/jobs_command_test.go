package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeJobs satisfies joblister without importing tools.
type fakeJobs struct{ jobs []Job }

func (f fakeJobs) ListJobs() []Job { return f.jobs }

func jobsHarness(t *testing.T, fj fakeJobs) (*Dispatcher, *Inbox, *fakeClient) {
	t.Helper()
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.SetJobs(fj)
	return d, inbox, c
}

// TestJobsCommand: /jobs renders the newest background jobs — human labels,
// no underscores (owner rule), newest first.
func TestJobsCommand(t *testing.T) {
	d, inbox, c := jobsHarness(t, fakeJobs{jobs: []Job{
		{ID: "j_old1", Kind: "download", Status: "done", Meta: map[string]string{"url": "https://x/a.csv"}, CreatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "j_new1", Kind: "download", Status: "running", Meta: map[string]string{"url": "https://x/b.csv"}, CreatedAt: time.Now().Add(-5 * time.Second)},
	}})
	d.Process(context.Background(), feed(t, inbox, 771, "/jobs"))

	out := lastSend(c)
	if !strings.Contains(out, "b.csv") {
		t.Fatalf("jobs text missing newest job url: %q", out)
	}
	if strings.Contains(out, "j_") {
		t.Fatalf("job ids must render without underscores: %q", out)
	}
	if !strings.Contains(out, "⏳") || !strings.Contains(out, "✅") {
		t.Fatalf("jobs text missing status icons: %q", out)
	}
	// Newest first: b.csv (5s ago) must appear before a.csv (2h ago).
	if strings.Index(out, "b.csv") > strings.Index(out, "a.csv") {
		t.Fatalf("jobs not newest-first: %q", out)
	}
}

// TestJobsCommandEmpty: an empty manager answers honestly, not with silence.
func TestJobsCommandEmpty(t *testing.T) {
	d, inbox, c := jobsHarness(t, fakeJobs{})
	d.Process(context.Background(), feed(t, inbox, 772, "/jobs"))

	if out := lastSend(c); !strings.Contains(out, "No background jobs") {
		t.Fatalf("empty jobs text = %q", out)
	}
}

// TestJobsCommandUnwired: no source wired degrades honestly — and the
// unknown-command guard must NOT claim it (it is a known command).
func TestJobsCommandUnwired(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.Process(context.Background(), feed(t, inbox, 773, "/jobs"))

	if out := lastSend(c); !strings.Contains(out, "unavailable") {
		t.Fatalf("unwired jobs text = %q", out)
	}
}

// TestJobsCappedAt8: a long history renders only the newest 8 rows.
func TestJobsCappedAt8(t *testing.T) {
	var jobs []Job
	for i := 0; i < 12; i++ {
		jobs = append(jobs, Job{ID: "j_pad", Kind: "download", Status: "done",
			Meta:      map[string]string{"url": "https://x/pad" + string(rune('a'+i)) + ".csv"},
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute)})
	}
	d, inbox, c := jobsHarness(t, fakeJobs{jobs: jobs})
	d.Process(context.Background(), feed(t, inbox, 774, "/jobs"))

	out := lastSend(c)
	if got := strings.Count(out, "—"); got != 8 {
		t.Fatalf("rendered rows = %d, want 8: %q", got, out)
	}
	if strings.Contains(out, "padl.csv") { // 12th-oldest must be dropped
		t.Fatalf("cap failed: oldest row still present: %q", out)
	}
}
