package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeCancel backs /cancel_job tests: records the id it was asked to stop
// and answers with the trio the real JobManager returns.
type fakeCancel struct {
	id      string
	known   bool
	issued  bool
	capture string
}

func (f *fakeCancel) CancelJob(id string) (Job, bool, bool) {
	f.capture = id
	return Job{ID: id, Kind: "download", Status: "running"}, f.known, f.issued
}

// TestCancelJobCommandIssued: cancelling a known running job tells the owner
// plainly that the stop was sent, id rendered bare.
func TestCancelJobCommandIssued(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.SetJobs(fakeJobs{jobs: []Job{{ID: "j_1", Kind: "download", Status: "running", CreatedAt: time.Now()}}})
	d.SetJobCancel(&fakeCancel{id: "j_1", known: true, issued: true})
	d.Process(context.Background(), feed(t, inbox, 781, "/cancel_job 1"))

	out := lastSend(c)
	if !strings.Contains(out, "🛑") || !strings.Contains(out, "1") {
		t.Fatalf("cancel-issued text = %q", out)
	}
	if strings.Contains(out, "j_") {
		t.Fatalf("id must render without underscore: %q", out)
	}
}

// TestCancelJobCommandUnknown: an id the manager never saw gets an honest
// unknown message, not a silent drop.
func TestCancelJobCommandUnknown(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.SetJobCancel(&fakeCancel{known: false})
	d.Process(context.Background(), feed(t, inbox, 782, "/cancel_job 404"))

	if out := lastSend(c); !strings.Contains(out, "Unknown job") {
		t.Fatalf("unknown text = %q", out)
	}
}

// TestCancelJobCommandAlreadyFinished: known but finished answers "nothing
// to cancel" instead of pretending a stop was issued.
func TestCancelJobCommandAlreadyFinished(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.SetJobCancel(&fakeCancel{known: true, issued: false})
	d.Process(context.Background(), feed(t, inbox, 783, "/cancel_job 9"))

	if out := lastSend(c); !strings.Contains(out, "already finished") {
		t.Fatalf("already-finished text = %q", out)
	}
}

// TestCancelJobCommandUsageAndUnwired: bare /cancel_job explains usage; a
// wired-less dispatcher reports unavailable rather than claiming the command.
func TestCancelJobCommandUsageAndUnwired(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	d.Process(context.Background(), feed(t, inbox, 784, "/cancel_job"))

	if out := lastSend(c); !strings.Contains(out, "Usage") {
		t.Fatalf("usage text = %q", out)
	}

	c2 := &fakeClient{}
	d2, inbox2 := harness(t, fakeEngine{answer: "x"}, c2)
	d2.Process(context.Background(), feed(t, inbox2, 785, "/cancel_job 1"))

	if out := lastSend(c2); !strings.Contains(out, "unavailable") {
		t.Fatalf("unwired text = %q", out)
	}
}

// TestCancelJobAcceptsUnderscoreSpelling: the owner may paste the full j_1
// form; both spellings reach the manager as the canonical id.
func TestCancelJobAcceptsUnderscoreSpelling(t *testing.T) {
	c := &fakeClient{}
	d, inbox := harness(t, fakeEngine{answer: "x"}, c)
	fc := &fakeCancel{known: true, issued: true}
	d.SetJobCancel(fc)
	d.Process(context.Background(), feed(t, inbox, 786, "/cancel_job j_2"))

	if fc.capture != "j_2" {
		t.Fatalf("manager saw %q, want j_2", fc.capture)
	}
	if out := lastSend(c); !strings.Contains(out, "🛑") {
		t.Fatalf("issued text = %q", out)
	}
}
