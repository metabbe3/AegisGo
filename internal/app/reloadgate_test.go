package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/app"
	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/telegram"
	"aegisgo/internal/tools"
)

// newTestRouter builds a router over the builtin toolset (seeded rules).
func newTestRouter(t *testing.T) *router.Router {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	set, err := tools.Builtin(tools.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(reg, router.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// fakeGateClient captures sends so the dispatcher can reply.
type fakeGateClient struct {
	telegram.Client // embed for the rest of the interface (unused methods)
	sends           []string
	buttons         [][]telegram.Button
}

func (f *fakeGateClient) SendMessage(_ context.Context, _ int64, text string, _ int64) (int64, error) {
	f.sends = append(f.sends, text)
	return int64(len(f.sends)), nil
}

// The gated flow, driven end to end with a real store + router:
// /reload_rules → approval row → human approves (simulated) → rules reloaded.
func TestReloadRulesGatedFlow(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := newTestRouter(t)
	g := &app.ReloadGate{Store: st, Router: rt}

	// Approve from "another thread" (the human on Telegram).
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pend, err := st.PendingApprovals(context.Background(), 5)
			if err == nil && len(pend) > 0 {
				st.DecideApproval(context.Background(), pend[0].ID, "approved", "test")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	out, outcome, err := g.Handle(context.Background(), "/reload_rules test")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome != tools.OutcomeApproved {
		t.Fatalf("outcome = %v, want approved", outcome)
	}
	m, ok := out.(map[string]any)
	if !ok || m["reloaded"] != true {
		t.Fatalf("out = %v", out)
	}
	if n, _ := m["rules"].(int); n == 0 {
		t.Fatal("reloaded zero rules")
	}
}

func TestReloadRulesDeniedKeepsRules(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := newTestRouter(t)
	g := &app.ReloadGate{Store: st, Router: rt}

	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pend, err := st.PendingApprovals(context.Background(), 5)
			if err == nil && len(pend) > 0 {
				st.DecideApproval(context.Background(), pend[0].ID, "denied", "test")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	out, outcome, err := g.Handle(context.Background(), "deny case")
	if err != nil || outcome != tools.OutcomeDenied || out != nil {
		t.Fatalf("deny path: out=%v outcome=%v err=%v", out, outcome, err)
	}
	// Rules table untouched: same defs still answer.
	d := rt.Handle(context.Background(), "/uptime")
	if !d.Handled {
		t.Fatal("router lost rules after deny — must keep previous set")
	}
}

// HandleText renders every outcome honestly (timeout branch here, fast).
func TestReloadRulesHandleTextTimeout(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := newTestRouter(t)
	// Shrink the wait so the test is fast: reuse Handle with a tiny config
	// by driving Request + RunGated directly through HandleText's gate.
	g := &app.ReloadGate{Store: st, Router: rt}
	g.QuickTimeout = 50 * time.Millisecond // test hook (see ReloadGate)
	gt := app.GatedTextFor(g)
	txt := gt.HandleText(context.Background(), "timeout case")
	if !strings.Contains(txt, "timed out") {
		t.Fatalf("timeout text wrong: %q", txt)
	}
}
