package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
)

// /register_rules (#65): paste-a-/rules-export gated batch import — the
// import twin of the export family (#60-#63). Every test here pins a
// semantic the gate promises: strict pre-approval parse, atomic batch,
// allowlist symmetry, idempotent replay, honest outcome text.

func newRegisterRulesGate(t *testing.T) (*RegisterRulesGate, *store.Store, *router.Router) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Real registry with the builtin tool set so Swap can accept read_doc
	// rules — the approved-path test proves the whole insert+swap chain.
	set, err := tools.Builtin(tools.Options{Workspace: repoRoot(t)})
	if err != nil {
		t.Fatalf("builtin set: %v", err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	rt, err := router.New(reg, nil)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return &RegisterRulesGate{Store: st, Router: rt, QuickTimeout: 200 * time.Millisecond}, st, rt
}

func TestParseRegisterRulesExtractsOnlyAddruleLines(t *testing.T) {
	// A real /rules export shape: header, replay lines, reference list.
	text := "Rules export (3):\n" +
		"/addrule name=disks | pattern=/disk|/df | tool=read_doc | args={\"path\":\"logs/disk.txt\"}\n" +
		"/addrule name=fc | pattern=/fc\\s+(\\S+) | tool=read_doc | args={\"path\":\"logs/x.log\"}\n" +
		"\n" +
		"Reference (not addable from chat):\n" +
		"/uptime → system_command\n"
	specs, err := parseRegisterRules(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2 (headers and reference lines skipped)", len(specs))
	}
	if specs[0].Name != "disks" || specs[1].Name != "fc" {
		t.Fatalf("names = %q %q", specs[0].Name, specs[1].Name)
	}
	if specs[0].Pattern != "/disk|/df" {
		t.Fatalf("pattern = %q, want alternation preserved", specs[0].Pattern)
	}
}

func TestParseRegisterRulesRejectsBadLineNoRow(t *testing.T) {
	// One invalid /addrule line rejects the WHOLE batch pre-approval —
	// batch atomicity; no approval row may exist afterwards.
	g, st, _ := newRegisterRulesGate(t)
	text := "/addrule name=good | pattern=/good | tool=read_doc\n" +
		"/addrule name=bad | pattern=notanchored | tool=read_doc\n"
	specs, err := parseRegisterRules(text)
	if err == nil {
		t.Fatal("parse accepted a bad line, want whole-batch reject")
	}
	if specs != nil {
		t.Fatalf("specs = %v, want nil on reject", specs)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Fatalf("error = %q, want line-quoted context", err)
	}
	// And the full gate flow must fail the same way WITHOUT minting a row.
	var rows int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM approvals`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("approvals = %d, want 0", rows)
	}
	if _, _, err := g.Handle(context.Background(), text); err == nil {
		t.Fatal("Handle accepted bad batch")
	}
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM approvals`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("rejected batch still created an approval row")
	}
}

func TestParseRegisterRulesUsageAndCap(t *testing.T) {
	if _, err := parseRegisterRules("/register_rules"); err == nil {
		t.Fatal("empty register accepted, want usage error")
	}
	if _, err := parseRegisterRules("just prose, no lines"); err == nil {
		t.Fatal("prose accepted, want usage error")
	}
	// 51 valid lines > maxRegisterLines cap (bounded-input rule #10 twin).
	var b strings.Builder
	b.WriteString("/register_rules\n")
	for i := 0; i < maxRegisterLines+1; i++ {
		b.WriteString("/addrule name=rulezz | pattern=/r | tool=read_doc\n")
	}
	if _, err := parseRegisterRules(b.String()); err == nil {
		t.Fatal("oversized batch accepted, want cap error")
	}
}

func TestRegisterRulesApprovedImportsBatch(t *testing.T) {
	g, st, rt := newRegisterRulesGate(t)
	text := "/addrule name=disks | pattern=/disk|/df | tool=read_doc | args={\"path\":\"logs/disk.txt\"}\n" +
		"/addrule name=fc | pattern=/fc\\s+(\\S+) | tool=read_doc | args={\"path\":\"logs/x.log\"}\n"

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

	out, outcome, err := g.Handle(context.Background(), text)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome != tools.OutcomeApproved {
		t.Fatalf("outcome = %v (%v), want approved", outcome, out)
	}
	m, ok := out.(map[string]any)
	if !ok || m["registered"] != 2 {
		t.Fatalf("out = %v, want registered=2", out)
	}
	// Rules landed in the table and the router now matches them.
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE origin='manual' AND enabled=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("manual rules = %d, want 2", n)
	}
	// Router actually serves one of the imported rules (insert+swap chain).
	dec := rt.Handle(context.Background(), "/disk")
	if !dec.Handled || dec.RuleID != "disks" {
		t.Fatalf("router after import = %+v, want disks rule hit", dec)
	}
}

func TestRegisterRulesDeniedImportsNothing(t *testing.T) {
	g, st, _ := newRegisterRulesGate(t)
	text := "/addrule name=disks | pattern=/disk|/df | tool=read_doc | args={\"path\":\"logs/disk.txt\"}"

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

	out, outcome, err := g.Handle(context.Background(), text)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome != tools.OutcomeDenied {
		t.Fatalf("outcome = %v (%v), want denied", outcome, out)
	}
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE name='disks'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("denied register still inserted a rule")
	}
}

func TestRegisterRulesReplayIsIdempotentUpsert(t *testing.T) {
	// Replaying a box's own export converges instead of duplicating.
	g, st, rt := newRegisterRulesGate(t)
	text := "/addrule name=disks | pattern=/disk|/df | tool=read_doc | args={\"path\":\"logs/disk.txt\"}"

	approveWhenPending := func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pend, err := st.PendingApprovals(context.Background(), 5)
			if err == nil && len(pend) > 0 {
				st.DecideApproval(context.Background(), pend[0].ID, "approved", "test")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		// No approval landed: Handle times out and the outcome assertion
		// below fails the test — never t.Fatal from this goroutine (vet).
	}

	for i := 0; i < 2; i++ {
		go approveWhenPending()
		out, outcome, err := g.Handle(context.Background(), text)
		if err != nil {
			t.Fatalf("round %d: handle: %v", i+1, err)
		}
		if outcome != tools.OutcomeApproved {
			t.Fatalf("round %d: outcome = %v, want approved", i+1, outcome)
		}
		if m, ok := out.(map[string]any); !ok || m["registered"] != 1 {
			t.Fatalf("round %d: out = %v", i+1, out)
		}
	}
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE name='disks'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replay produced %d rows, want 1 (upsert)", n)
	}
	dec := rt.Handle(context.Background(), "/disk")
	if !dec.Handled || dec.RuleID != "disks" {
		t.Fatalf("router after replay = %+v, want disks rule hit", dec)
	}
}

func TestRegisterRulesRunRejectsNonAddableTool(t *testing.T) {
	// parseRegisterRules itself allows only addable tools via parseAddRule,
	// but Run re-validates (defense in depth) — a direct call with a
	// smuggled spec must fail before any insert.
	g, st, _ := newRegisterRulesGate(t)
	spec := &ruleSpec{Name: "evil", Pattern: "/evil", Tool: "system_command", Args: "{}"}
	if _, err := g.Run(context.Background(), []*ruleSpec{spec}); err == nil {
		t.Fatal("Run accepted system_command spec")
	}
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE name='evil'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("non-addable Run still inserted")
	}
}

func TestRegisterRulesRequestReasonTruncates(t *testing.T) {
	// 6 specs: the reason lists 5 then "+1 more" — the approver sees a
	// bounded preview, not a wall (bounded-output rule #10).
	g, st, _ := newRegisterRulesGate(t)
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString("/addrule name=rulezz | pattern=/r | tool=read_doc\n")
	}
	specs, err := parseRegisterRules(b.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	id, err := g.Request(context.Background(), specs)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	row, ok, err := st.GetApproval(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get approval: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(row.Reason, "register 6 rule(s)") || !strings.Contains(row.Reason, "+1 more") {
		t.Fatalf("reason = %q, want count + truncation marker", row.Reason)
	}
}

func TestRegisterRulesRunDefaultsEmptyArgsAndFailsSwapWithoutRegistry(t *testing.T) {
	// A spec with NO args must default to "{}" on insert, and a Swap
	// against a registry without the tool must reject honestly (rows stay
	// for /reload_rules retry) — the addrule Run contract, batch edition.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := tools.NewRegistry() // empty REAL registry: Swap rejects tool
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	rt, err := router.New(reg, nil)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	g := &RegisterRulesGate{Store: st, Router: rt}
	spec := &ruleSpec{Name: "bare", Pattern: "/bare(?:\\s+(?<p>.*))?", Tool: "read_doc"} // no Args
	if _, err := g.Run(context.Background(), []*ruleSpec{spec}); err == nil {
		t.Fatal("Run without registry tools should fail Swap")
	}
	var args string
	if err := st.QueryRow(context.Background(), `SELECT args_template FROM rules WHERE name='bare'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if args != "{}" {
		t.Fatalf("args_template = %q, want {} default", args)
	}
}

func TestExistingApprovalDelegatesDecide(t *testing.T) {
	// existingApproval adapts *store.Store to tools.ApprovalLedger for
	// gates that pre-create their row; the decide path must delegate 1:1
	// (wired for interface completeness — pinned so drift is caught).
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	id, err := st.CreateApproval(ctx, "register_rules", "{}", "test")
	if err != nil {
		t.Fatal(err)
	}
	lv := &existingApproval{st: st, id: id}
	ok, err := lv.DecideApproval(ctx, id, "denied", "test")
	if err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}
	row, found, err := lv.GetApproval(ctx, id)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if row.State != "denied" {
		t.Fatalf("state = %q, want denied", row.State)
	}
}

func TestRegisterRulesHandleTextBranches(t *testing.T) {
	g, _, _ := newRegisterRulesGate(t)
	rt := registerRulesText{g: g}

	if got := rt.HandleText(context.Background(), "/register_rules"); got == "" || !strings.Contains(got, "usage") {
		t.Fatalf("usage text = %q", got)
	}
	if got := rt.HandleText(context.Background(),
		"/addrule name=zz | pattern=oops | tool=read_doc"); !strings.Contains(got, "must start with") {
		t.Fatalf("parse-error text = %q", got)
	}
	// Bare gate (nobody decides): timeout branch after QuickTimeout.
	got := rt.HandleText(context.Background(), "/addrule name=zz | pattern=/zz | tool=read_doc")
	if !strings.Contains(got, "timed out") && !strings.Contains(got, "denied") {
		t.Fatalf("undecided text = %q, want timeout/deny", got)
	}
}
