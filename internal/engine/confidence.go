// Package engine — confidence.go
//
// Decision confidence for every run (owner directive 28 Sep 2026: "make
// decision engine also like confidence level", modelled on the Hermes
// verifier gate). Deterministic scoring — NOT an LLM judge — per the
// Evolution Coordinator's research finding (28 Sep): the most precise
// layer is deterministic verification recomputed from state.
//
// Bands mirror the Hermes verifier's recommendation ladder:
//
//	80-100 HIGH   ship as-is
//	60-79  MEDIUM ship with a note
//	0-59   LOW    treat as lead, verify before trusting
//
// Scores by decision_source:
//
//	regex_router    100 — deterministic rule + native tool, no model in the path
//	llm_classifier   90 — model picked a NATIVE tool; the answer itself is
//	                      deterministic tool output (risk = wrong tool)
//	llm              55 base + up to +30 from verifiable signals:
//	                   +10  answer cites a rule ID the router knows
//	                 +8    router has rules covering this prompt family
//	                       (the miner corpus grows coverage over time)
//	                 +6 per distinct tool call the model actually made
//	                       (grounded in real output, capped +12)
//	                 +4    response is structured (lists/code/tables —
//	                       deliberate work, not a one-liner guess)
//	llm_disabled      0 — refusal, not an answer
//	error             0 — failure, not an answer
package engine

import (
	"regexp"

	"aegisgo/internal/router"
	"aegisgo/internal/store"
)

// Confidence bands (exported for headers, tests, and any interface that
// wants to color-code the answer).
const (
	ConfHighMin   = 80
	ConfMediumMin = 60
)

// Confidence is the scored result attached to every Result.
type Confidence struct {
	Score int    // 0-100
	Band  string // HIGH | MEDIUM | LOW
	Why   string // one-line human explanation of the dominant signal
}

func bandOf(score int) string {
	switch {
	case score >= ConfHighMin:
		return "HIGH"
	case score >= ConfMediumMin:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// sigRuleRef matches "[via rule-id]" / "rule <id>" / bare "/cmd" references
// an answer may carry when it leaned on router capabilities.
var sigRuleRef = regexp.MustCompile(`\b(?:via\s+)?(rule[-/][A-Za-z0-9_-]+|\^(?:uptime|help|reload_rules|stats))`)

// sigStructured marks deliberate multi-part answers (steps, bullets, code,
// tables) — cheap proxy for "worked the problem" vs "guessed a one-liner".
var sigStructured = regexp.MustCompile(`(?m)^\s*(?:[-*]\s|\d+[.)]\s|` + "```" + `)`)

// ScoreRouter is the confidence of a deterministic router hit.
func ScoreRouter() Confidence {
	return Confidence{Score: 100, Band: "HIGH", Why: "deterministic rule + native tool"}
}

// ScoreClassifier is the confidence of a fast-tier native-tool pick: the
// output is deterministic; the residual risk is tool choice.
func ScoreClassifier() Confidence {
	return Confidence{Score: 90, Band: "HIGH", Why: "model-routed, deterministic tool output"}
}

// ScoreLLM computes confidence for an LLM fallback answer from verifiable
// signals. ruleCount is the router's live rule count; toolsUsed is the
// number of DISTINCT tool calls the model made (from the response's tool
// annotations); answer is the final text.
func ScoreLLM(answer string, toolsUsed, ruleCount int) Confidence {
	score := 55
	why := "model judgment, unverified"
	switch {
	case toolsUsed >= 2:
		score += 12
		why = "grounded in 2+ tool results"
	case toolsUsed == 1:
		score += 6
		why = "grounded in 1 tool result"
	}
	if sigRuleRef.MatchString(answer) {
		score += 10
		why += " + cites router rule"
	}
	if ruleCount > 0 {
		score += 8 // coverage exists for this prompt family; mining grows it
	}
	if sigStructured.MatchString(answer) {
		score += 4
	}
	if score > 100 {
		score = 100
	}
	return Confidence{Score: score, Band: bandOf(score), Why: why}
}

// ScoreNone covers refusals and failures: not answers, zero confidence.
func ScoreNone(why string) Confidence {
	return Confidence{Score: 0, Band: "LOW", Why: why}
}

// scoreForSource rebuilds a confidence for sources whose answer never went
// through the LLM scoring path (audit replay, tests).
func scoreForSource(src string) Confidence {
	switch src {
	case store.SourceRouter:
		return ScoreRouter()
	case store.SourceLLMClassifier:
		return ScoreClassifier()
	default:
		return ScoreNone("no verifiable signal")
	}
}

// ruleCountSafe returns the router's rule count without panicking on a nil
// router (tests construct bare engines).
func ruleCountSafe(r *router.Router) int {
	if r == nil {
		return 0
	}
	return len(r.RuleDefs())
}

// distinctToolCount counts DISTINCT tool names (repeated calls to the same
// tool ground one signal, not three).
func distinctToolCount(names []string) int {
	seen := map[string]bool{}
	for _, n := range names {
		if n != "" {
			seen[n] = true
		}
	}
	return len(seen)
}
