// Package router is the deterministic half of AegisGo's hybrid decision
// engine. Rules are anchored, precompiled regexes; a match executes a
// registered Go tool natively and answers immediately — no LLM, no cost,
// no latency beyond the tool itself. Go's regexp engine is linear-time, so
// patterns cannot backtrack-explode, and prompts are length-capped anyway.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"aegisgo/internal/tools"
)

// MaxPrompt bounds router input; longer prompts skip straight to the LLM
// path (they are not command-shaped).
const MaxPrompt = 4 * 1024

// Rule lifecycle states. Seeded rules are active; mined rules enter as
// shadow (learn before they answer) and promote on agreement — or demote
// on any divergence. Demoted rules are disabled.
const (
	RuleActive  = "active"
	RuleShadow  = "shadow"
	RuleDemoted = "demoted"
)

// RuleDef is the declarative form of a rule — what the rules table stores
// and what the Phase-3 miner writes.
type RuleDef struct {
	Name string // rule id, e.g. "uptime" or "mined_3"
	// Pattern is an unanchored regex; the router anchors it as ^(?:pattern)$.
	Pattern string
	// Tool is the registered tool name to execute on match.
	Tool string
	// ArgsTemplate is the tool's JSON args with "$1", "$2"… spliced from
	// capture groups, e.g. `{"path":"$1"}`.
	ArgsTemplate string
	// Origin is "seed" or "mined".
	Origin string
	// State is RuleActive (default), RuleShadow, or RuleDemoted.
	State string
}

// EffectiveState defaults empty to active (pre-v3 rows, in-code seeds).
func (d RuleDef) EffectiveState() string {
	if d.State == "" {
		return RuleActive
	}
	return d.State
}

// CompiledRule is a RuleDef ready to match.
type CompiledRule struct {
	RuleDef
	re *regexp.Regexp
}

// Compile validates and anchors a RuleDef's pattern.
func (d RuleDef) Compile() (CompiledRule, error) { return d.compile() }

// Regexp exposes the anchored pattern (tests, admin tooling).
func (c *CompiledRule) Regexp() *regexp.Regexp { return c.re }

func (r RuleDef) compile() (CompiledRule, error) {
	re, err := regexp.Compile(`^(?:` + r.Pattern + `)$`)
	if err != nil {
		return CompiledRule{}, fmt.Errorf("rule %s: %w", r.Name, err)
	}
	if r.Tool == "" {
		return CompiledRule{}, fmt.Errorf("rule %s: tool is required", r.Name)
	}
	return CompiledRule{RuleDef: r, re: re}, nil
}

// Router matches prompts against a hot-swappable rule set and executes the
// winning rule's tool.
type Router struct {
	rules   atomic.Pointer[[]CompiledRule]
	toolset *tools.Registry
}

// New builds a router from defs, verifying every rule names a registered
// tool (a rule pointing at a missing tool is a routing bug: fail loudly).
func New(toolset *tools.Registry, defs []RuleDef) (*Router, error) {
	compiled := make([]CompiledRule, 0, len(defs))
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if seen[d.Name] {
			return nil, fmt.Errorf("duplicate rule %q", d.Name)
		}
		seen[d.Name] = true
		if _, ok := toolset.Get(d.Tool); !ok {
			return nil, fmt.Errorf("rule %q references unknown tool %q", d.Name, d.Tool)
		}
		c, err := d.compile()
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, c)
	}
	r := &Router{toolset: toolset}
	r.rules.Store(&compiled)
	return r, nil
}

// Swap atomically replaces the rule set (hot reload).
func (r *Router) Swap(defs []RuleDef) error {
	compiled := make([]CompiledRule, 0, len(defs))
	for _, d := range defs {
		if _, ok := r.toolset.Get(d.Tool); !ok {
			return fmt.Errorf("rule %q references unknown tool %q", d.Name, d.Tool)
		}
		c, err := d.compile()
		if err != nil {
			return err
		}
		compiled = append(compiled, c)
	}
	r.rules.Store(&compiled)
	return nil
}

// Decision is the outcome of routing one prompt.
type Decision struct {
	// Handled is true when a rule matched and the tool ran.
	Handled bool
	// RuleID names the matched rule.
	RuleID string
	// Output is the tool's typed result.
	Output any
	// Err carries tool execution failures (Handled is still true — the
	// rule matched; the tool failed).
	Err error

	// Evaluate requests the shadow comparison: the engine ALSO runs the
	// LLM and records tool-choice agreement in shadow_events. Set for
	// shadow-state rules (always) and active mined rules (1% sampling).
	Evaluate bool
	// AnswerFromRule selects whose answer wins when Evaluate is set:
	// true (sampled active rule — the rule answers, LLM only checks) or
	// false (shadow rule — the LLM answers while the rule learns).
	AnswerFromRule bool

	// Tool and Args expose the matched invocation for the comparison.
	Tool string
	Args []byte
}

// Text renders the tool output as the reply body.
func (d Decision) Text() string {
	if d.Output == nil {
		return ""
	}
	b, err := json.MarshalIndent(d.Output, "", "  ")
	if err != nil {
		return fmt.Sprint(d.Output)
	}
	return string(b)
}

// sampleActiveMined is the 1% permanent-sampling decision for promoted
// mined rules — the auto-demotion guard. A variable (not a const) purely
// as the deterministic injection point for tests.
var sampleActiveMined = func() bool { return rand.Intn(100) == 0 }

// SetSampleForTest swaps the sampler and returns the previous one.
// Production code never calls this; tests force the 1% branch deterministically.
func SetSampleForTest(f func() bool) func() bool {
	prev := sampleActiveMined
	sampleActiveMined = f
	return prev
}

// SampleForTest exposes the current sampler (test assertions).
func SampleForTest() func() bool { return sampleActiveMined }

// Handle routes one prompt. Matching rules execute their tool inline; the
// caller decides what a miss means (LLM fallback lives in internal/engine).
func (r *Router) Handle(ctx context.Context, prompt string) Decision {
	if len(prompt) > MaxPrompt {
		return Decision{}
	}
	rules := *r.rules.Load()
	for _, rule := range rules {
		match := rule.re.FindStringSubmatch(prompt)
		if match == nil {
			continue
		}
		args := spliceArgs(rule.ArgsTemplate, match)
		tl, ok := r.toolset.Get(rule.Tool)
		if !ok {
			// Toolset and rules drifted (rules hot-swapped against a
			// different registry). Report, don't crash.
			return Decision{Handled: true, RuleID: rule.Name,
				Err: fmt.Errorf("tool %q vanished", rule.Tool)}
		}
		out, err := tl.Execute(ctx, args)

		d := Decision{Handled: true, RuleID: rule.Name, Output: out, Err: err,
			Tool: rule.Tool, Args: args}
		switch rule.EffectiveState() {
		case RuleShadow:
			// Learn: LLM answers, rule is compared.
			d.Evaluate, d.AnswerFromRule = true, false
		case RuleActive:
			if rule.Origin == "mined" && sampleActiveMined() {
				// Guard: rule answers, LLM double-checks 1% of traffic.
				d.Evaluate, d.AnswerFromRule = true, true
			}
		}
		return d
	}
	return Decision{}
}

// spliceArgs substitutes "$N" captures into the args template. The quoted
// form "$1" splices JSON-escaped content; the bare form $1 splices the raw
// capture and is only safe where the rule's regex guarantees its shape
// (e.g. \d+ for numbers).
func spliceArgs(template string, match []string) []byte {
	out := template
	for i := 1; i < len(match); i++ {
		escaped, _ := json.Marshal(match[i]) // includes surrounding quotes
		ref := "$" + strconv.Itoa(i)
		// Quoted form first: "$1" → fully escaped value with quotes kept.
		out = strings.ReplaceAll(out, `"`+ref+`"`, string(escaped))
		// Bare form: $1 → raw capture (safe only for shape-validated groups).
		out = strings.ReplaceAll(out, ref, match[i])
	}
	return []byte(out)
}

// RuleDefs returns the live rule set in declarative form (for /stats and
// admin tooling later).
func (r *Router) RuleDefs() []RuleDef {
	live := *r.rules.Load()
	out := make([]RuleDef, 0, len(live))
	for _, c := range live {
		out = append(out, c.RuleDef)
	}
	return out
}
