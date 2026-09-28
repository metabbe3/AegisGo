package engine

import (
	"strings"
	"testing"
)

// Decision-engine confidence (owner directive 28 Sep 2026): every
// decision_source maps to a deterministic score; bands mirror the Hermes
// verifier ladder (HIGH ≥80, MEDIUM ≥60, else LOW).

func TestScoreRouterDeterministic(t *testing.T) {
	c := ScoreRouter()
	if c.Score != 100 || c.Band != "HIGH" {
		t.Fatalf("router confidence = %+v, want 100/HIGH", c)
	}
}

func TestScoreClassifier(t *testing.T) {
	c := ScoreClassifier()
	if c.Score != 90 || c.Band != "HIGH" {
		t.Fatalf("classifier confidence = %+v, want 90/HIGH", c)
	}
}

func TestScoreLLMBands(t *testing.T) {
	cases := []struct {
		name         string
		answer       string
		tools, rules int
		wantScore    int
		wantBand     string
	}{
		{"bare one-liner, no coverage", "yes", 0, 0, 55, "LOW"},
		{"one tool grounds", "used read_csv", 1, 0, 61, "MEDIUM"},
		{"two tools ground more", "a", 2, 0, 67, "MEDIUM"},
		{"rule coverage adds", "a", 0, 12, 63, "MEDIUM"},
		{"cites rule adds", "see rule-csv-1", 0, 0, 65, "MEDIUM"},
		{"structured adds", "- step one\n- step two", 0, 0, 59, "LOW"},
		{"full stack", "via rule-x\n- a\n- b", 3, 30, 89, "HIGH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreLLM(tc.answer, tc.tools, tc.rules)
			if got.Score != tc.wantScore || got.Band != tc.wantBand {
				t.Fatalf("ScoreLLM(%q,%d,%d) = %d/%s, want %d/%s",
					tc.answer, tc.tools, tc.rules, got.Score, got.Band, tc.wantScore, tc.wantBand)
			}
		})
	}
}

func TestScoreNone(t *testing.T) {
	c := ScoreNone("refusal, not an answer")
	if c.Score != 0 || c.Band != "LOW" {
		t.Fatalf("ScoreNone = %+v, want 0/LOW", c)
	}
}

func TestHeaderCarriesConfidenceForNonHigh(t *testing.T) {
	r := Result{DecisionSource: "llm", LatencyMS: 12, Confidence: Confidence{Score: 55, Band: "LOW"}}
	h := r.Header(", ")
	if !strings.Contains(h, "LOW conf=55") {
		t.Fatalf("header %q missing confidence", h)
	}
	r.Confidence = Confidence{Score: 100, Band: "HIGH"}
	if strings.Contains(r.Header(", "), "conf=") {
		t.Fatalf("HIGH header should stay bare, got %q", r.Header(", "))
	}
}

func TestDistinctToolCount(t *testing.T) {
	if got := distinctToolCount([]string{"read_csv", "read_csv", "csv_stats", ""}); got != 2 {
		t.Fatalf("distinctToolCount = %d, want 2", got)
	}
}
