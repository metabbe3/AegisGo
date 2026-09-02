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

// Periodic calls fn on its own goroutine every interval. Each call gets a
// fresh context bounded by timeout, deliberately derived from
// context.Background rather than ctx: the parent may outlive many ticks,
// but one slow tick must never outlive its own deadline, and an in-flight
// tick is allowed to finish after stop/ctx-cancel — only the next tick is
// prevented. The loop exits when ctx is canceled or stop is called; stop
// is idempotent, because cleanup closures fire from more than one exit
// path and a bare channel close would panic on the second call (same
// contract as store.Close). interval <= 0 disables the loop entirely.
func Periodic(ctx context.Context, interval, timeout time.Duration, fn func(ctx context.Context)) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
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
	return func() { once.Do(func() { close(done) }) }
}
