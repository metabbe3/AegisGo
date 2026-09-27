package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJobCancelRunning: cancelling a running job stops its fn via the
// context, and the final status is the honest "cancelled" — not a cryptic
// "context canceled" error string.
func TestJobCancelRunning(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), 30*time.Second, "download",
		map[string]string{"url": "https://x/f.bin"}, func(ctx context.Context) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

	j, known, issued := mgr.Cancel(id)
	if !known || !issued {
		t.Fatalf("cancel running: known=%v issued=%v, want true/true", known, issued)
	}
	if j.Status != JobRunning {
		t.Fatalf("snapshot at cancel time = %q, want running (flip is async)", j.Status)
	}

	final := waitForJob(t, mgr, id)
	if final.Status != JobCancelled {
		t.Fatalf("final status = %q, want cancelled", final.Status)
	}
	if final.Error != "cancelled" {
		t.Fatalf("error = %q, want the plain word cancelled", final.Error)
	}
}

// TestJobCancelAlreadyFinished: cancelling a done job is an honest no-op,
// never an error and never a status change.
func TestJobCancelAlreadyFinished(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), 5*time.Second, "k", nil,
		func(ctx context.Context) (any, error) { return "r", nil })
	waitForJob(t, mgr, id)

	_, known, issued := mgr.Cancel(id)
	if !known || issued {
		t.Fatalf("cancel finished: known=%v issued=%v, want true/false", known, issued)
	}
	if j, _ := mgr.Get(id); j.Status != JobDone {
		t.Fatalf("status changed by no-op cancel: %+v", j)
	}
}

// TestJobCancelUnknown: an id the manager never minted reports unknown.
func TestJobCancelUnknown(t *testing.T) {
	mgr := NewJobManager()
	_, known, issued := mgr.Cancel("j_nope")
	if known || issued {
		t.Fatalf("cancel unknown: known=%v issued=%v, want false/false", known, issued)
	}
}

// TestJobCancelTwice: a second cancel of the same job is a no-op (the
// wrapper already consumed the cancel entry).
func TestJobCancelTwice(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), 30*time.Second, "k", nil,
		func(ctx context.Context) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	if _, _, issued := mgr.Cancel(id); !issued {
		t.Fatal("first cancel must issue")
	}
	final := waitForJob(t, mgr, id)
	_, known, issued := mgr.Cancel(id)
	if !known || issued {
		t.Fatalf("second cancel: known=%v issued=%v, want true/false (status %q)", known, issued, final.Status)
	}
}

// TestJobDownloadCancelEndToEnd: the download tool's real fetch honors a
// cancel — the httptest server parks, cancel fires, the job lands on
// "cancelled" and no complete file exists at the target.
func TestJobDownloadCancelEndToEnd(t *testing.T) {
	gotReq := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq <- struct{}{}
		<-release
		fmt.Fprint(w, "too late")
	}))
	defer srv.Close()
	defer close(release)

	ws := t.TempDir()
	mgr := NewJobManager()
	tl, err := NewDownload(ws, mgr, DownloadOptions{AllowPrivate: true, TimeoutSecs: 30})
	if err != nil {
		t.Fatal(err)
	}

	res, err := tl.Execute(context.Background(), []byte(`{"url":"`+srv.URL+`/slow","path":"c/f.bin"}`))
	if err != nil {
		t.Fatal(err)
	}
	jobID := res.(DownloadJobOutput).JobID
	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the request")
	}

	_, known, issued := mgr.Cancel(jobID)
	if !known || !issued {
		t.Fatalf("cancel: known=%v issued=%v", known, issued)
	}
	final := waitForJob(t, mgr, jobID)
	if final.Status != JobCancelled {
		t.Fatalf("final = %q, want cancelled", final.Status)
	}
	// The final path never appeared (the staged .part was cleaned up).
	if _, err := os.Stat(filepath.Join(ws, "c", "f.bin")); !os.IsNotExist(err) {
		t.Errorf("final file exists after cancel: %v", err)
	}
}
