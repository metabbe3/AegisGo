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

// /addrule from Telegram (owner directive 28 Sep): parse-strict, gated L2,
// allowlisted tools, hot-swap after approve.

func newAddRuleGate(t *testing.T) (*AddRuleGate, *store.Store, *router.Router) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Empty REAL registry (no tools registered): Swap then rejects the
	// rule's tool name honestly instead of panicking on a nil registry.
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	rt, err := router.New(reg, nil)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return &AddRuleGate{Store: st, Router: rt, QuickTimeout: 50 * time.Millisecond}, st, rt
}

func TestParseAddRuleValid(t *testing.T) {
	spec, err := parseAddRule(`/addrule name=myhelp | pattern=/myhelp(?:\s+(?<arg>.*))? | tool=read_doc | args={"path":"docs/$arg"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Name != "myhelp" || spec.Tool != "read_doc" {
		t.Fatalf("spec = %+v", spec)
	}
	if !strings.Contains(spec.Reason, `add rule "myhelp"`) {
		t.Fatalf("reason = %q", spec.Reason)
	}
}

func TestParseAddRuleRejects(t *testing.T) {
	bad := []string{
		`/addrule`, // empty
		`/addrule name=x | pattern=hello | tool=read_doc`,             // pattern not slash-anchored
		`/addrule name=x | pattern=/x | tool=read_doc | args=notjson`, // args not JSON
		`/addrule name=Bad Name | pattern=/x | tool=read_doc`,         // invalid name chars
		`/addrule pattern=/x | tool=read_doc`,                         // missing name
		`/addrule name=x | pattern=/x[unclosed | tool=read_doc`,       // regex does not compile
		`/addrule name=x | pattern=/x | tool=read_doc | extra=1`,      // unknown key
	}
	for _, in := range bad {
		if _, err := parseAddRule(in); err == nil {
			t.Errorf("parseAddRule(%q) accepted, want reject", in)
		}
	}
}

func TestParseAddRuleRejectsCatchAll(t *testing.T) {
	// A pattern without the leading slash would swallow every message.
	if _, err := parseAddRule(`/addrule name=evil | pattern=.* | tool=read_doc`); err == nil {
		t.Fatal("catch-all pattern accepted")
	}
}

func TestAddRuleToolAllowlist(t *testing.T) {
	for _, ok := range []string{"read_csv", "csv_stats", "read_doc"} {
		if !addableTool(ok) {
			t.Errorf("addableTool(%q) = false", ok)
		}
	}
	for _, no := range []string{"system_command", "sql_query", "download", ""} {
		if addableTool(no) {
			t.Errorf("addableTool(%q) = true", no)
		}
	}
}

func TestAddRuleDeniedChangesNothing(t *testing.T) {
	g, st, _ := newAddRuleGate(t)
	out, outcome, err := g.Handle(context.Background(),
		`/addrule name=nope | pattern=/nope | tool=read_doc`)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome != tools.OutcomeDenied && outcome != tools.OutcomeTimeout {
		t.Fatalf("outcome = %v (%v), want denied/timeout — nothing decides in a bare test", outcome, out)
	}
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE name='nope'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("denied/timeout add_rule still inserted a rule")
	}
}

func TestAddRuleRunInsertsAndFailsSwapWithoutRegistry(t *testing.T) {
	g, st, _ := newAddRuleGate(t)
	spec := &ruleSpec{Name: "doc", Pattern: "/doc(?:\\s+(?<p>.*))?", Tool: "read_doc", Args: `{"path":"$p"}`}
	// Run with a registry-less router: Swap must reject the unknown tool,
	// and the honest error keeps the row (documented behavior).
	if _, err := g.Run(context.Background(), spec); err == nil {
		t.Fatal("Run without registry should fail Swap")
	}
	var n int
	if err := st.QueryRow(context.Background(), `SELECT COUNT(*) FROM rules WHERE name='doc' AND origin='manual'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("rule row missing after Run (row should persist for /reload_rules retry)")
	}
}

// HandleText is the chat-facing surface: every branch must render honest
// human text — parse error with usage, approve confirmation, deny, timeout.
func TestAddRuleHandleTextBranches(t *testing.T) {
	g, _, _ := newAddRuleGate(t)
	at := addRuleText{g: g}

	if got := at.HandleText(context.Background(), `/addrule name=zz | pattern=oops | tool=read_doc`); got == "" || !strings.Contains(got, "must start with") {
		t.Fatalf("parse-error text = %q", got)
	}
	if got := at.HandleText(context.Background(), `/addrule name=zz | pattern=/zz | tool=sql_query`); !strings.Contains(got, "not addable") {
		t.Fatalf("allowlist text = %q", got)
	}
	// Bare gate (nobody decides): timeout branch after QuickTimeout.
	got := at.HandleText(context.Background(), `/addrule name=zz | pattern=/zz | tool=read_doc`)
	if !strings.Contains(got, "timed out") && !strings.Contains(got, "denied") {
		t.Fatalf("undecided text = %q, want timeout/deny", got)
	}
}
