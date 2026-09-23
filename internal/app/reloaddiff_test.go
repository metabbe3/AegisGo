package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
)

// diffRules: added/removed/changed lines, sorted, human words only.
func TestDiffRulesHumanLines(t *testing.T) {
	live := []router.RuleDef{
		{Name: "uptime", Pattern: "uptime", Tool: "system_command"},
		{Name: "old", Pattern: "old", Tool: "read_csv"},
	}
	next := []router.RuleDef{
		{Name: "uptime", Pattern: "uptime2", Tool: "system_command"}, // changed
		{Name: "fresh", Pattern: "new", Tool: "read_csv"},            // added
	}
	got := diffRules(live, next)
	for _, want := range []string{"+ fresh → read_csv", "− old", "~ uptime"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff missing %q:\n%s", want, got)
		}
	}
}

// Identical sets render as "no rule changes".
func TestDiffRulesNoChange(t *testing.T) {
	same := []router.RuleDef{{Name: "uptime", Pattern: "u", Tool: "system_command"}}
	if got := diffRules(same, same); got != "no rule changes" {
		t.Fatalf("want no-change line, got %q", got)
	}
}

// TestReloadDiffHandleFlow: the diff-preview gate runs end to end — request
// carries the rule diff in the reason; an approval swaps the router.
func TestReloadDiffHandleFlow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Exec(context.Background(),
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts, state)
		 VALUES ('uptime','^/uptime$','system_command','{}','seed',1,'2026-09-01T00:00:00Z','active')`); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
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
	g := &ReloadDiffGate{ReloadGate: &ReloadGate{Store: st, Router: rt, QuickTimeout: 2 * time.Second}}

	// Approver thread: watch the ledger, approve the first pending row.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pend, err := st.PendingApprovals(context.Background(), 5)
			if err == nil && len(pend) > 0 {
				st.DecideApproval(context.Background(), pend[0].ID, "approved", "test")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	res, outcome, err := g.Handle(context.Background(), "preview test")
	if err != nil || outcome != tools.OutcomeApproved {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	_ = res
}

// TestReloadDiffRequestCarriesDiff: RequestWithDiff embeds "Rule changes:"
// in the approval reason when the stored rules differ from live.
func TestReloadDiffRequestCarriesDiff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	wd, _ := os.Getwd()
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
	g := &ReloadDiffGate{ReloadGate: &ReloadGate{Store: st, Router: rt}}
	id, err := g.RequestWithDiff(context.Background(), "diff test")
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := st.GetApproval(context.Background(), id)
	if !ok {
		t.Fatal("approval row missing")
	}
	if !strings.Contains(row.Reason, "Rule changes:") {
		t.Fatalf("reason lacks diff preview: %q", row.Reason)
	}
}
