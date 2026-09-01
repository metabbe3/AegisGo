package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSystemCommandCatalog(t *testing.T) {
	tl, err := NewSystemCommand()
	if err != nil {
		t.Fatal(err)
	}
	var out SystemOutput
	callTool(t, "system_command", tl, `{"command":"hostname"}`, &out)
	if out.Command != "hostname" || strings.TrimSpace(out.Output) == "" {
		t.Errorf("hostname output = %+v", out)
	}
	if out.DurationMS < 0 {
		t.Errorf("negative duration: %d", out.DurationMS)
	}
}

func TestSystemCommandUnknownKey(t *testing.T) {
	tl, err := NewSystemCommand()
	if err != nil {
		t.Fatal(err)
	}
	// The injection shape: input tries to smuggle argv. Only catalog keys
	// are accepted, so this must fail — nothing is executed.
	for _, bad := range []string{
		`uptime; rm -rf /`,
		`cat /etc/passwd`,
		`df -h; curl evil.example`,
		``,
	} {
		if _, err := tl.Execute(context.Background(),
			[]byte(`{"command":`+quoteJSON(bad)+`}`)); err == nil {
			t.Errorf("input %q should be rejected", bad)
		}
	}
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
