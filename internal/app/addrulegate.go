package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/telegram"
	"aegisgo/internal/tools"
)

// AddRuleGate is the second real L2 gated action (owner directive 28 Sep:
// "add rules/command from telegram"). Writing a router rule changes agent
// behavior at runtime — same L2 class as reload_rules (ADR-0007): the
// approval ledger row records the EXACT rule being added, and the ✅/🚫
// buttons on Telegram decide it.
//
// Syntax (one message, pipe-separated):
//
//	/addrule name=myhelp | pattern=/myhelp(?:\s+(?<arg>.*))? | tool=read_doc | args={"path":"docs/$arg"}
//
// Only path-arg tools are addable from chat (Hard Rule 12 symmetry: the
// miner never proposes system_command/sql_query either — a chat-added
// rule must not widen the command surface). The rule lands in state
// active with origin "manual"; approval is per-rule (reason carries the
// parsed rule so the human sees exactly what ✅ will do).
type AddRuleGate struct {
	Store  *store.Store
	Router *router.Router
	// QuickTimeout overrides the wait for tests (0 = default 10 min).
	QuickTimeout time.Duration
}

// GateConfigFor mirrors ReloadGate: humans answer on their phone.
func (g *AddRuleGate) GateConfig() tools.GateConfig {
	wait := 10 * time.Minute
	if g.QuickTimeout > 0 {
		wait = g.QuickTimeout
	}
	return tools.GateConfig{WaitTimeout: wait, PollInterval: 10 * time.Millisecond}
}

// ruleSpec is the parsed /addrule payload shared by validation, the
// approval reason, and Run.
type ruleSpec struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Tool    string `json:"tool"`
	Args    string `json:"args,omitempty"`
	Reason  string `json:"reason"`
}

// parseAddRule parses the pipe-separated text after "/addrule ". Strict on
// purpose: a typo'd pattern must fail BEFORE any approval row exists.
func parseAddRule(text string) (*ruleSpec, error) {
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/addrule"))
	if body == "" {
		return nil, fmt.Errorf("usage: /addrule name=X | pattern=Y | tool=Z | args={...} — see /help")
	}
	spec := &ruleSpec{}
	for _, part := range strings.Split(body, "|") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad segment %q (want key=value)", strings.TrimSpace(part))
		}
		k, v := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1])
		switch k {
		case "name":
			spec.Name = v
		case "pattern":
			spec.Pattern = v
		case "tool":
			spec.Tool = v
		case "args":
			spec.Args = v
		default:
			return nil, fmt.Errorf("unknown key %q (want name|pattern|tool|args)", k)
		}
	}
	if spec.Name == "" || spec.Pattern == "" || spec.Tool == "" {
		return nil, fmt.Errorf("name, pattern, and tool are required")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`).MatchString(spec.Name) {
		return nil, fmt.Errorf("name %q must be lowercase letters/digits/underscore", spec.Name)
	}
	// An injective pattern is dangerous: it would swallow EVERY message.
	// Require an explicit anchor on the command word.
	trimmed := strings.TrimSpace(spec.Pattern)
	if !strings.HasPrefix(trimmed, "/") {
		return nil, fmt.Errorf("pattern must start with \"/\" (chat rules are commands, not catch-alls)")
	}
	if _, err := regexp.Compile("^(?:" + spec.Pattern + ")$"); err != nil {
		return nil, fmt.Errorf("pattern does not compile: %v", err)
	}
	if spec.Args != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(spec.Args), &probe); err != nil {
			return nil, fmt.Errorf("args must be a JSON object: %v", err)
		}
	}
	spec.Reason = fmt.Sprintf("add rule %q → tool %s, pattern %s", spec.Name, spec.Tool, spec.Pattern)
	return spec, nil
}

// addableTools is the allowlist (Hard Rule 12 symmetry). mapset as slice
// check — three entries, a map is overkill.
func addableTool(t string) bool {
	switch t {
	case "read_csv", "csv_stats", "read_doc", "analyze_log":
		return true
	}
	return false
}

// Request creates the approval row carrying the parsed rule as reason.
func (g *AddRuleGate) Request(ctx context.Context, spec *ruleSpec) (int64, error) {
	payload, err := tools.MarshalPayload(map[string]string{
		"action": "add_rule", "name": spec.Name, "pattern": spec.Pattern,
		"tool": spec.Tool, "args": spec.Args,
	})
	if err != nil {
		return 0, err
	}
	return g.Store.CreateApproval(ctx, "add_rule", payload, spec.Reason)
}

// Run inserts the rule and hot-swaps the router once a human approved.
// The tool-name check happens here (not in parse) because the registry is
// runtime state.
func (g *AddRuleGate) Run(ctx context.Context, spec *ruleSpec) (any, error) {
	if !addableTool(spec.Tool) {
		return nil, fmt.Errorf("add_rule: tool %q not addable from chat (allowed: read_csv, csv_stats, read_doc, analyze_log)", spec.Tool)
	}
	args := spec.Args
	if args == "" {
		args = "{}"
	}
	if err := g.Store.Exec(ctx,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, state, enabled, created_ts)
		 VALUES (?,?,?,?, 'manual', 'active', 1, ?)
		 ON CONFLICT(name) DO UPDATE SET pattern=excluded.pattern, tool=excluded.tool,
		   args_template=excluded.args_template, enabled=1`,
		spec.Name, spec.Pattern, spec.Tool, args,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("add_rule: insert: %w", err)
	}
	defs, err := router.LoadRules(ctx, g.Store)
	if err != nil {
		return nil, fmt.Errorf("add_rule: reload: %w", err)
	}
	if err := g.Router.Swap(defs); err != nil {
		return nil, fmt.Errorf("add_rule: swap rejected (rule kept in table, fix and /reload_rules): %w", err)
	}
	return map[string]any{"added": spec.Name, "rules": len(defs)}, nil
}

// Handle is the full gated flow: parse → ledger → wait → execute.
func (g *AddRuleGate) Handle(ctx context.Context, text string) (any, tools.GateOutcome, error) {
	spec, err := parseAddRule(text)
	if err != nil {
		return nil, "", err // parse errors surface pre-approval, no row created
	}
	if !addableTool(spec.Tool) {
		return nil, "", fmt.Errorf("add_rule: tool %q not addable from chat (allowed: read_csv, csv_stats, read_doc, analyze_log)", spec.Tool)
	}
	id, err := g.Request(ctx, spec)
	if err != nil {
		return nil, "", err
	}
	lv := &existingApproval{st: g.Store, id: id}
	run := func(ctx context.Context) (any, error) { return g.Run(ctx, spec) }
	return tools.RunGated(ctx, lv, g.GateConfig(), "add_rule", "{}", spec.Reason, run)
}

// addRuleText adapts AddRuleGate to telegram.GatedAction. Parse/validation
// errors are typed out so chat shows the usage line instead of a scary
// "failed".
type addRuleText struct{ g *AddRuleGate }

func (a addRuleText) HandleText(ctx context.Context, text string) string {
	out, outcome, err := a.g.Handle(ctx, text)
	switch {
	case err != nil:
		if strings.Contains(err.Error(), "usage:") || strings.Contains(err.Error(), "must be") ||
			strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "compile") {
			return "⚠️ " + err.Error()
		}
		return "⚠️ add_rule failed: " + err.Error()
	case outcome == tools.OutcomeApproved:
		return fmt.Sprintf("✅ rule added & live: %v", out)
	case outcome == tools.OutcomeDenied:
		return "🚫 add_rule denied — nothing changed."
	case outcome == tools.OutcomeTimeout:
		return "⏱ add_rule approval timed out — nothing changed. Re-send to retry."
	default:
		return fmt.Sprintf("add_rule outcome: %s (%v)", outcome, out)
	}
}

var _ telegram.GatedAction = addRuleText{}
