package loop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond every ~2ms until it holds or the deadline passes,
// then fails the test. Timing is observed, never assumed via sleeps.
func waitFor(t *testing.T, cond func() bool, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", deadline)
}

// TestPeriodicFires: ticks call fn repeatedly until stopped.
func TestPeriodicFires(t *testing.T) {
	var n atomic.Int32
	stop := Periodic(context.Background(), 2*time.Millisecond, time.Second,
		func(context.Context) { n.Add(1) })
	defer stop()
	waitFor(t, func() bool { return n.Load() >= 3 }, 2*time.Second)
}

// TestPeriodicStopIsIdempotentAndEndsTicks pins the BC4 contract: calling
// stop twice must not panic, and no new ticks fire afterwards. An in-flight
// tick may still complete, but fn here is a single atomic add, so the
// 30ms quiet windows make a straggler observationally impossible.
func TestPeriodicStopIsIdempotentAndEndsTicks(t *testing.T) {
	var n atomic.Int32
	stop := Periodic(context.Background(), 2*time.Millisecond, time.Second,
		func(context.Context) { n.Add(1) })
	waitFor(t, func() bool { return n.Load() > 0 }, 2*time.Second)
	stop()
	stop() // the whole point: second call is a no-op, not a panic
	time.Sleep(30 * time.Millisecond)
	after := n.Load()
	time.Sleep(30 * time.Millisecond)
	if got := n.Load(); got != after {
		t.Errorf("ticks continued after stop: %d then %d", after, got)
	}
}

// TestPeriodicExitsOnContextCancel: canceling the parent ends ticking — a
// canceled boot context leaves no goroutine working against a store the
// caller is about to close.
func TestPeriodicExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	Periodic(ctx, 2*time.Millisecond, time.Second, func(context.Context) { n.Add(1) })
	waitFor(t, func() bool { return n.Load() > 0 }, 2*time.Second)
	cancel()
	time.Sleep(30 * time.Millisecond)
	after := n.Load()
	time.Sleep(30 * time.Millisecond)
	if got := n.Load(); got != after {
		t.Errorf("ticks continued after ctx cancel: %d then %d", after, got)
	}
}

// TestPeriodicBindsTickTimeout: fn's context carries a deadline bounded by
// the configured timeout — one slow tick can never run forever.
func TestPeriodicBindsTickTimeout(t *testing.T) {
	ctxs := make(chan context.Context, 1)
	stop := Periodic(context.Background(), 2*time.Millisecond, time.Minute,
		func(ctx context.Context) {
			select {
			case ctxs <- ctx:
			default:
			}
		})
	defer stop()
	select {
	case ctx := <-ctxs:
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("tick context has no deadline")
		}
		if r := time.Until(dl); r <= 0 || r > time.Minute {
			t.Errorf("tick deadline remainder = %v, want within (0, %v]", r, time.Minute)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fn never fired")
	}
}

// TestPeriodicDisabled: interval <= 0 is the documented off switch — no
// goroutine, no ticks, stop still safe to call.
func TestPeriodicDisabled(t *testing.T) {
	var n atomic.Int32
	stop := Periodic(context.Background(), 0, time.Second, func(context.Context) { n.Add(1) })
	stop()
	stop()
	time.Sleep(10 * time.Millisecond)
	if n.Load() != 0 {
		t.Errorf("fn fired %d times with interval 0", n.Load())
	}
}
