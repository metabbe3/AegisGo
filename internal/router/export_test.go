package router

import "testing"

// TestChatAddableAllowlist: the export dialect only replays read-only,
// path-arg tools (Hard Rule 12 symmetry) — system_command and sql_query
// must stay reference-only forever.
func TestChatAddableAllowlist(t *testing.T) {
	for _, tool := range []string{"read_csv", "csv_stats", "read_doc", "analyze_log"} {
		if !ChatAddable(tool) {
			t.Errorf("ChatAddable(%q) = false, want true", tool)
		}
	}
	for _, tool := range []string{"system_command", "sql_query", "ops_forecast", "", "download"} {
		if ChatAddable(tool) {
			t.Errorf("ChatAddable(%q) = true, want false", tool)
		}
	}
}

// TestExportNormalizesArgs: empty/whitespace ArgsTemplate renders as "{}"
// (what /addrule replay expects) and State falls back to active for legacy
// rows via EffectiveState.
func TestExportNormalizesArgs(t *testing.T) {
	out := Export([]RuleDef{
		{Name: "a", Pattern: "/a", Tool: "read_csv", ArgsTemplate: "   ", Origin: "manual"},
		{Name: "b", Pattern: "/b", Tool: "read_csv", ArgsTemplate: `{"path":"$1"}`, Origin: "mined"},
	})
	if len(out) != 2 {
		t.Fatalf("Export returned %d rules, want 2", len(out))
	}
	if out[0].ArgsTemplate != "{}" {
		t.Errorf("out[0].ArgsTemplate = %q, want {} (normalized)", out[0].ArgsTemplate)
	}
	if out[0].State != "active" {
		t.Errorf("out[0].State = %q, want active (EffectiveState fallback)", out[0].State)
	}
	if !out[0].ChatAddable || !out[1].ChatAddable {
		t.Errorf("read_csv rules must be chat_addable, got %v/%v", out[0].ChatAddable, out[1].ChatAddable)
	}
}

// TestReplayLineDialect: the rendered line is exactly the dialect
// /addrule parses (name | pattern | tool | args, pipes re-joined per #61);
// non-addable tools decline with ok=false.
func TestReplayLineDialect(t *testing.T) {
	d := RuleDef{Name: "mined_1", Pattern: `err|FATAL`, Tool: "read_csv", ArgsTemplate: `{"path":"$1"}`}
	line, ok := d.ReplayLine()
	if !ok {
		t.Fatal("ReplayLine ok = false for a chat-addable rule")
	}
	want := `name=mined_1 | pattern=err|FATAL | tool=read_csv | args={"path":"$1"}`
	if line != want {
		t.Errorf("ReplayLine = %q, want %q", line, want)
	}
	sys := RuleDef{Name: "uptime", Pattern: "/uptime", Tool: "system_command"}
	if _, ok := sys.ReplayLine(); ok {
		t.Error("ReplayLine ok = true for system_command, want false (reference-only)")
	}
}
