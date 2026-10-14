package app

import (
	"context"
	"testing"
	"time"

	"aegisgo/internal/tools"
)

// jobsSource adapts *tools.JobManager to the dispatcher's joblister. The
// adapter must preserve every field jobsText reads (id, kind, status, meta,
// createdAt) — driving the manager through its real Start path pins the
// mapping against future tools.Job changes.
func TestJobsSourceAdaptsManager(t *testing.T) {
	m := tools.NewJobManager()
	src := jobsSource{m}

	if got := src.ListJobs(); len(got) != 0 {
		t.Fatalf("empty manager returned %d jobs", len(got))
	}

	// Seed one job through the manager's real Start: fn parks until released
	// so the running state is observable deterministically.
	release := make(chan struct{})
	id := m.Start(context.Background(), time.Minute, "download",
		map[string]string{"url": "https://example.com/a.csv"},
		func(ctx context.Context) (any, error) {
			<-release
			return "saved", nil
		})

	running := src.ListJobs()
	if len(running) != 1 {
		t.Fatalf("running snapshot: got %d jobs, want 1", len(running))
	}
	j := running[0]
	if j.ID != id {
		t.Fatalf("id = %q, want %q", j.ID, id)
	}
	if j.Kind != "download" || j.Status != "running" {
		t.Fatalf("kind/status = %q/%q, want download/running", j.Kind, j.Status)
	}
	if j.Meta["url"] != "https://example.com/a.csv" {
		t.Fatalf("meta url lost: %v", j.Meta)
	}
	if time.Since(j.CreatedAt) > time.Minute {
		t.Fatalf("createdAt not carried: %v", j.CreatedAt)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if js := src.ListJobs(); len(js) == 1 && js[0].Status == "done" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached done: %+v", src.ListJobs())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// CancelJob must map the manager's three-way result (job, known, issued)
// through the adapter without losing fields — the /jobs cancel path
// renders all of them.
func TestJobsSourceCancelMapsThreeWayResult(t *testing.T) {
	m := tools.NewJobManager()
	src := jobsSource{m}

	// Unknown id: known=false, no panic, zero-value job.
	if _, known, issued := src.CancelJob("nope"); known || issued {
		t.Fatalf("unknown id: known=%v issued=%v, want false/false", known, issued)
	}

	// A running job cancels: known + issued, fields carried. The body must
	// select on ctx.Done() — Cancel fires the context; a job that ignores
	// it (plain <-release) stays running forever, and real bodies (the
	// download tool) are ctx-respecting by contract.
	release := make(chan struct{})
	id := m.Start(context.Background(), time.Minute, "download",
		map[string]string{"url": "https://example.com/big.csv"},
		func(ctx context.Context) (any, error) {
			select {
			case <-release:
				return "saved", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	j, known, issued := src.CancelJob(id)
	if !known || !issued {
		t.Fatalf("running id: known=%v issued=%v, want true/true", known, issued)
	}
	if j.ID != id || j.Kind != "download" {
		t.Fatalf("mapped job = %+v", j)
	}
	// Cancellation is asynchronous (the wrapper goroutine is the single
	// writer of job outcomes): poll briefly for the terminal cancelled
	// status — pinned via the tools constant, not a re-typed literal, so
	// the test can never disagree with the manager's spelling again.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if js := src.ListJobs(); len(js) == 1 && js[0].Status == tools.JobCancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never cancelled: %+v", src.ListJobs())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
