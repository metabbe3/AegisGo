package telegram

import (
	"strings"
	"testing"
)

func TestRenderPayloadHumanSentence(t *testing.T) {
	got := renderPayload(`{"command":"reload_rules","path":"/tmp/x.csv"}`)
	if strings.Contains(got, "{") || strings.Contains(got, "\"") || strings.Contains(got, "_") {
		t.Fatalf("payload not human: %q", got)
	}
	if !strings.Contains(got, "Command") || !strings.Contains(got, "reload rules") {
		t.Fatalf("want key/value words: %q", got)
	}
}

func TestRenderPayloadEmpty(t *testing.T) {
	if got := renderPayload(`{}`); got != "" {
		t.Fatalf("empty object should be blank, got %q", got)
	}
}

func TestRenderPayloadPlainString(t *testing.T) {
	got := renderPayload(`just-a-command`)
	if got != "just-a-command" {
		t.Fatalf("plain passthrough: %q", got)
	}
}

func TestHumanKindNoUnderscores(t *testing.T) {
	if got := humanKind("system_command"); got != "System command" {
		t.Fatalf("kind = %q", got)
	}
}

// The chat must never show raw JSON for approvals.
func TestApprovalLineNeverRawJSON(t *testing.T) {
	a := ApprovalInfo{ID: 3, Kind: "system_command",
		Payload: `{"command":"uptime"}`, Reason: "check host"}
	line := formatApprovalLine(a)
	if strings.Contains(line, `{"`) {
		t.Fatalf("raw JSON leaked: %q", line)
	}
	if !strings.Contains(line, "Command uptime") {
		t.Fatalf("missing human payload: %q", line)
	}
}
