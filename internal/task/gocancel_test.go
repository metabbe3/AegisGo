package task

import (
	"context"
	"testing"
	"time"
)

// TestGoCancelCancelsOnDemand: the cancel handed back by GoCancel must
// actually cancel the goroutine's context while the timeout stays the
// hard bound (the whole point of the variant).
func TestGoCancelCancelsOnDemand(t *testing.T) {
	var g Group
	entered := make(chan struct{})
	canceled := make(chan struct{})
	cancel := g.GoCancel(context.Background(), 10*time.Second, func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(canceled)
	})
	<-entered
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel() did not cancel the goroutine context")
	}
	if !g.Wait(time.Second) {
		t.Fatal("group did not drain after cancel")
	}
}

// TestGoCancelIdempotentLate: firing cancel after the goroutine finished
// must not panic or hang — callers may hold it indefinitely.
func TestGoCancelIdempotentLate(t *testing.T) {
	var g Group
	done := make(chan struct{})
	cancel := g.GoCancel(context.Background(), 10*time.Second, func(ctx context.Context) {
		close(done)
	})
	<-done
	_ = g.Wait(time.Second)
	cancel() // late fire: must be a safe no-op
	cancel() // and again
}

// TestGoCancelTimeoutStillBounds: without any cancel call, the timeout
// still expires the context (Go semantics unchanged).
func TestGoCancelTimeoutStillBounds(t *testing.T) {
	var g Group
	expired := make(chan struct{})
	_ = g.GoCancel(context.Background(), 30*time.Millisecond, func(ctx context.Context) {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			close(expired)
		}
	})
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout bound lost in GoCancel")
	}
}
