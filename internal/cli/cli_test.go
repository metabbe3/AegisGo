// Tests for the aegis subcommand dispatcher itself: usage, unknown
// commands, version, help, and end-to-end flag dispatch into agent/ctl.
package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestMainNoArgsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), nil, &out, &errBuf); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "usage: aegis") {
		t.Errorf("stderr = %q, want usage", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on usage error", out.String())
	}
}

func TestMainUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Main(context.Background(), []string{"frobnicate"}, &out, &errBuf)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want unknown-command wording", errBuf.String())
	}
}

func TestMainVersion(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), []string{"version"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := out.String(); got != "aegis "+Version+"\n" {
		t.Errorf("version output = %q, want %q", got, "aegis "+Version+"\n")
	}
}

func TestMainHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var out, errBuf bytes.Buffer
		if code := Main(context.Background(), []string{arg}, &out, &errBuf); code != 0 {
			t.Errorf("%s: exit = %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "usage: aegis <command>") {
			t.Errorf("%s: stdout = %q, want the usage text", arg, out.String())
		}
	}
}

// TestMainAgentFlagDispatch drives the full subcommand grammar end to end:
// flags parsed by the subcommand's FlagSet, remaining args as the prompt,
// answered by the offline router.
func TestMainAgentFlagDispatch(t *testing.T) {
	agentBaseEnv(t)
	var out, errBuf bytes.Buffer
	code := Main(context.Background(), []string{"agent", "--tier", "smart", "/hostname"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	hn, _ := os.Hostname()
	if !strings.Contains(out.String(), hn) {
		t.Errorf("output = %q, want hostname %q", out.String(), hn)
	}
}

func TestMainAgentBadTier(t *testing.T) {
	agentBaseEnv(t)
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), []string{"agent", "--tier", "bogus", "/x"}, &out, &errBuf); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), `unknown tier "bogus"`) {
		t.Errorf("stderr = %q, want unknown-tier failure", errBuf.String())
	}
}

func TestMainAgentUnknownFlag(t *testing.T) {
	agentBaseEnv(t)
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), []string{"agent", "--wat", "x"}, &out, &errBuf); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	// The flag package normalizes to single-dash in its error message.
	if !strings.Contains(errBuf.String(), "flag provided but not defined: -wat") {
		t.Errorf("stderr = %q, want the flag error", errBuf.String())
	}
}

// TestMainAgentHelpFlag: -h on a subcommand prints its flag defaults and
// exits 0 (flag.ErrHelp).
func TestMainAgentHelpFlag(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), []string{"agent", "-h"}, &out, &errBuf); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(errBuf.String(), "-tier") {
		t.Errorf("stderr = %q, want flag defaults", errBuf.String())
	}
}

func TestMainCtlDispatch(t *testing.T) {
	withDB(t, nil)
	var out, errBuf bytes.Buffer
	if code := Main(context.Background(), []string{"ctl", "stats"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"total_runs"`) {
		t.Errorf("stats output = %q, want total_runs", out.String())
	}
}

// TestMainCoversShimPath keeps the smoke contract the cmd/aegis shim relies
// on: version works with a Discard stderr too.
func TestMainVersionQuiet(t *testing.T) {
	var out bytes.Buffer
	if code := Main(context.Background(), []string{"version"}, &out, io.Discard); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}
