package task

import (
	"context"
	"testing"
	"time"
)

// TestGoDetachesFromParentCancel: the whole reason the package exists —
// canceling the request that started a task must not kill the task.
func TestGoDetachesFromParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	var g Group
	g.Go(parent, 10*time.Second, func(ctx context.Context) {
		defer close(ran)
		select {
		case <-time.After(20 * time.Millisecond): // outlives the parent
		case <-ctx.Done():
			t.Error("task context canceled together with parent")
		}
	})
	cancel()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not run to completion after parent cancel")
	}
}

// TestGoKeepsParentValues: detachment drops cancellation, not values —
// trace IDs ride along.
func TestGoKeepsParentValues(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "trace-1")
	got := make(chan any, 1)
	var g Group
	g.Go(parent, time.Second, func(ctx context.Context) { got <- ctx.Value(key{}) })
	select {
	case v := <-got:
		if v != "trace-1" {
			t.Errorf("task ctx value = %v, want trace-1", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task never ran")
	}
}

// TestGoBindsTimeout: fn's context carries a deadline bounded by the
// configured timeout — no task runs forever.
func TestGoBindsTimeout(t *testing.T) {
	ctxs := make(chan context.Context, 1)
	var g Group
	g.Go(context.Background(), time.Minute, func(ctx context.Context) { ctxs <- ctx })
	select {
	case ctx := <-ctxs:
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("task context has no deadline")
		}
		if r := time.Until(dl); r <= 0 || r > time.Minute {
			t.Errorf("task deadline remainder = %v, want within (0, %v]", r, time.Minute)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task never ran")
	}
}

// TestWaitReportsDrain: Wait is the shutdown join — true when everything
// finished, false when the bound elapsed with work still running.
func TestWaitReportsDrain(t *testing.T) {
	var g Group
	release := make(chan struct{})
	g.Go(context.Background(), 10*time.Second, func(context.Context) { <-release })

	if g.Wait(10 * time.Millisecond) {
		t.Error("Wait = true with a blocked task, want false")
	}
	close(release)
	if !g.Wait(2 * time.Second) {
		t.Error("Wait = false after release, want true")
	}
}

// TestWaitOnEmptyGroup: no work means an immediate drain — a serve
// shutdown with zero in-flight async runs must not stall.
func TestWaitOnEmptyGroup(t *testing.T) {
	var g Group
	if !g.Wait(time.Second) {
		t.Error("Wait = false on empty group, want true")
	}
}
