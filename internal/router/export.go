package router

import "strings"

// export.go owns the rule-export dialect — the ONE place that knows (a)
// which tools are replayable via /addrule (the AddRuleGate allowlist,
// Hard Rule 12 symmetry) and (b) how a rule renders as a /addrule replay
// line. Three surfaces consume it (Telegram #60, REST #62, ctl #63);
// before this module each carried its own copy of the allowlist — the
// third duplication is when the repo extracts (CLAUDE.md style rule).

// ChatAddable reports whether a rule's tool is replayable from chat via
// /addrule (read-only, path-arg tools only). system_command & co. ship
// with the binary's Seeded() — replaying them from chat is impossible by
// design (allowlist symmetry); exports list them as reference-only.
func ChatAddable(tool string) bool {
	switch tool {
	case "read_csv", "csv_stats", "read_doc", "analyze_log":
		return true
	}
	return false
}

// ExportedRule is one live rule in machine-readable form — the payload of
// GET /v1/rules/export (#62). ArgsTemplate is normalized to "{}" when
// empty, matching what /addrule replay expects; State defaults to active
// for legacy rows via EffectiveState.
type ExportedRule struct {
	Name         string `json:"name"`
	Pattern      string `json:"pattern"`
	Tool         string `json:"tool"`
	ArgsTemplate string `json:"args_template"`
	Origin       string `json:"origin"`
	State        string `json:"state"`
	ChatAddable  bool   `json:"chat_addable"`
}

// Export renders defs as ExportedRules — shared by the REST handler and
// any future consumer that wants the structured form.
func Export(defs []RuleDef) []ExportedRule {
	out := make([]ExportedRule, 0, len(defs))
	for _, d := range defs {
		args := strings.TrimSpace(d.ArgsTemplate)
		if args == "" {
			args = "{}"
		}
		out = append(out, ExportedRule{
			Name:         d.Name,
			Pattern:      d.Pattern,
			Tool:         d.Tool,
			ArgsTemplate: args,
			Origin:       d.Origin,
			State:        d.EffectiveState(),
			ChatAddable:  ChatAddable(d.Tool),
		})
	}
	return out
}

// ReplayLine renders one rule as a /addrule line — the dialect rules are
// replayed in on a fresh box (#60). Returns ok=false for tools that are
// not chat-addable (callers list those as reference-only, below).
func (d RuleDef) ReplayLine() (string, bool) {
	if !ChatAddable(d.Tool) {
		return "", false
	}
	args := strings.TrimSpace(d.ArgsTemplate)
	if args == "" {
		args = "{}"
	}
	return "name=" + d.Name + " | pattern=" + d.Pattern + " | tool=" + d.Tool + " | args=" + args, true
}
