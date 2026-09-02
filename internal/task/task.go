// Package task runs detached background work — goroutines that must
// outlive the request that started them but not the process — and lets
// shutdown join them with a real bound instead of a guessed sleep.
package task

import (
	"context"
	"sync"
	"time"
)

// Group tracks goroutines started with Go. The zero value is ready to use
// and copied-by-value use is wrong (Wait must see every Go) — share one
// pointer, like a sync.WaitGroup.
type Group struct {
	wg sync.WaitGroup
}

// Go runs fn on its own goroutine, tracked by the group. fn's context is
// detached from parent's cancellation (context.WithoutCancel) but keeps
// its values (trace IDs) and is bounded by timeout: a background task must
// outlive the HTTP request that started it, yet never run forever.
func (g *Group) Go(parent context.Context, timeout time.Duration, fn func(ctx context.Context)) {
	g.wg.Add(1)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	go func() {
		defer g.wg.Done()
		defer cancel()
		fn(ctx)
	}()
}

// Wait blocks until every goroutine started with Go has finished, or d
// elapses, and reports whether the group drained. Call from shutdown
// paths after the servers have stopped accepting new work.
func (g *Group) Wait(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	return WaitFor(done, d)
}

// WaitFor blocks until done is closed or d elapses and reports whether the
// join happened — the bounded-join idiom behind Group.Wait, shared by any
// component that owns its own goroutine-exit channel (e.g. telegram's
// PollLoop). One timer, no helper goroutine, so an abandoned wait leaks
// nothing.
func WaitFor(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
