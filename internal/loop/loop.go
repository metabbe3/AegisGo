// Package loop runs periodic background work with one shared shape: a
// ticker whose every tick gets its own bounded context, an arm on the
// parent context, and an idempotent stop. It exists so the miner and the
// rules hot-reload (and whatever comes next) share one tested loop instead
// of hand-rolled tickers.
package loop

import (
	"context"
	"sync"
	"time"
)

// stopGrace bounds how long stop waits for an in-flight tick: fast ticks
// (the normal case) finish well inside it, a pathological one is abandoned
// to its own per-tick timeout.
const stopGrace = 2 * time.Second

// Periodic calls fn on its own goroutine every interval. Each call gets a
// fresh context bounded by timeout, deliberately derived from
// context.Background rather than ctx: the parent may outlive many ticks,
// but one slow tick must never outlive its own deadline. The loop exits
// when ctx is canceled or stop is called. stop is idempotent (cleanup
// closures fire from more than one exit path and a bare channel close
// would panic on the second call — same contract as store.Close) and
// joins the goroutine for up to stopGrace, so a fast in-flight tick
// completes before stop returns instead of racing the caller's teardown —
// e.g. writing into a store the caller is about to close. interval <= 0
// disables the loop entirely.
func Periodic(ctx context.Context, interval, timeout time.Duration, fn func(ctx context.Context)) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				tctx, cancel := context.WithTimeout(context.Background(), timeout)
				fn(tctx)
				cancel()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(done)
			select {
			case <-exited:
			case <-time.After(stopGrace):
			}
		})
	}
}
