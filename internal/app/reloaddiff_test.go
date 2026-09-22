package app

import (
	"strings"
	"testing"

	"aegisgo/internal/router"
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
