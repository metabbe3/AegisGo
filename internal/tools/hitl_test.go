package tools

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// fakeLedger implements ApprovalLedger with scriptable state transitions.
type fakeLedger struct {
	nextID  int64
	created []string // kinds created
	// states maps approval id -> state, consulted on every GetApproval;
	// a test can flip it from another goroutine to simulate a human.
	states map[int64]string
}

func (f *fakeLedger) CreateApproval(ctx context.Context, kind, payload, reason string) (int64, error) {
	f.nextID++
	if f.states == nil {
		f.states = map[int64]string{}
	}
	f.states[f.nextID] = "pending"
	f.created = append(f.created, kind)
	return f.nextID, nil
}

func (f *fakeLedger) GetApproval(ctx context.Context, id int64) (ApprovalRow, bool, error) {
	st, ok := f.states[id]
	if !ok {
		return ApprovalRow{}, false, nil
	}
	return ApprovalRow{ID: id, State: st}, true, nil
}

func (f *fakeLedger) DecideApproval(ctx context.Context, id int64, state, by string) (bool, error) {
	if _, ok := f.states[id]; !ok {
		return false, nil
	}
	f.states[id] = state
	return true, nil
}

// decideNext flips the newest pending row to state (simulates the human
// pressing ✅/🚫 while the gate polls).
func (f *fakeLedger) decideNext(state string) {
	for i := 0; i < 100; i++ {
		for id, st := range f.states {
			if st == "pending" {
				f.states[id] = state
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fastGate() GateConfig {
	return GateConfig{WaitTimeout: 500 * time.Millisecond, PollInterval: 5 * time.Millisecond}
}

func TestRunGatedApprovedExecutes(t *testing.T) {
	led := &fakeLedger{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		led.states[1] = "approved" // the human clicks
	}()
	ran := false
	out, outcome, err := RunGated(t.Context(), led, fastGate(), "system_command", `{"command":"x"}`, "L2 test",
		func(ctx context.Context) (any, error) { ran = true; return "done", nil })
	if err != nil || outcome != OutcomeApproved || !ran || out != "done" {
		t.Errorf("approved path wrong: out=%v outcome=%v ran=%v err=%v", out, outcome, ran, err)
	}
	if len(led.created) != 1 || led.created[0] != "system_command" {
		t.Errorf("created = %v, want one system_command", led.created)
	}
}

func TestRunGatedDeniedNeverExecutes(t *testing.T) {
	led := &fakeLedger{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		led.states[1] = "denied"
	}()
	ran := false
	out, outcome, err := RunGated(t.Context(), led, fastGate(), "k", "{}", "r",
		func(ctx context.Context) (any, error) { ran = true; return nil, nil })
	if err != nil || outcome != OutcomeDenied || ran || out != nil {
		t.Errorf("denied path wrong: out=%v outcome=%v ran=%v err=%v", out, outcome, ran, err)
	}
}

func TestRunGatedExpiredNeverExecutes(t *testing.T) {
	led := &fakeLedger{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		led.states[1] = "expired"
	}()
	out, outcome, _ := RunGated(t.Context(), led, fastGate(), "k", "{}", "r",
		func(ctx context.Context) (any, error) { return "should not run", nil })
	if outcome != OutcomeExpired || out != nil {
		t.Errorf("expired path wrong: out=%v outcome=%v", out, outcome)
	}
}

func TestRunGatedTimeoutNeverExecutes(t *testing.T) {
	led := &fakeLedger{} // stays pending forever
	ran := false
	_, outcome, _ := RunGated(t.Context(), led, fastGate(), "k", "{}", "r",
		func(ctx context.Context) (any, error) { ran = true; return nil, nil })
	if outcome != OutcomeTimeout || ran {
		t.Errorf("timeout path wrong: outcome=%v ran=%v", outcome, ran)
	}
}

func TestRunGatedShutdownNeverExecutes(t *testing.T) {
	led := &fakeLedger{}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	ran := false
	_, outcome, _ := RunGated(ctx, led, fastGate(), "k", "{}", "r",
		func(ctx context.Context) (any, error) { ran = true; return nil, nil })
	if outcome != OutcomeShutdown || ran {
		t.Errorf("shutdown path wrong: outcome=%v ran=%v", outcome, ran)
	}
}

func TestRunGatedPropagatesFnError(t *testing.T) {
	led := &fakeLedger{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		led.states[1] = "approved"
	}()
	wantErr := fmt.Errorf("boom")
	_, outcome, err := RunGated(t.Context(), led, fastGate(), "k", "{}", "r",
		func(ctx context.Context) (any, error) { return nil, wantErr })
	if outcome != OutcomeApproved || err != wantErr {
		t.Errorf("fn error must propagate: outcome=%v err=%v", outcome, err)
	}
}

func TestMarshalPayload(t *testing.T) {
	got, err := MarshalPayload(map[string]string{"command": "restart"})
	if err != nil || got != `{"command":"restart"}` {
		t.Errorf("MarshalPayload = %q err=%v", got, err)
	}
}

// TestDescribeGatedDryRun: the dry-run path returns the payload on approval
// and NEVER executes a real action (the side-effect probe stays untouched).
func TestDescribeGatedDryRun(t *testing.T) {
	l := &fakeLedger{}
	go l.decideNext("approved")
	got, outcome, err := DescribeGated(context.Background(), l, GateConfig{WaitTimeout: 2 * time.Second, PollInterval: 20 * time.Millisecond},
		"system_command", `{"command":"disk"}`, "dry-run probe")
	if err != nil || outcome != OutcomeApproved {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	if got != `{"command":"disk"}` {
		t.Fatalf("preview = %q", got)
	}
	// Dry-run contract: DescribeGated has no action parameter at all —
	// there is nothing that COULD execute. The preview IS the payload.
}

// TestDescribeGatedDeny: a denied dry-run returns the deny outcome, empty payload.
func TestDescribeGatedDeny(t *testing.T) {
	l := &fakeLedger{}
	go l.decideNext("denied")
	got, outcome, err := DescribeGated(context.Background(), l, GateConfig{WaitTimeout: 2 * time.Second, PollInterval: 20 * time.Millisecond},
		"k", "{}", "r")
	if err != nil || outcome != OutcomeDenied || got != "" {
		t.Fatalf("got=%q outcome=%v err=%v", got, outcome, err)
	}
}
