package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpBaseEnv mirrors agentBaseEnv for mcp-server subcommand tests: offline,
// hermetic, temp store + workspace.
func mcpBaseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AEGIS_MCP_SERVERS", "AEGIS_TELEGRAM_TOKEN", "AEGIS_GRPC_ADDR",
		"AEGIS_MCP_TOKEN",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AEGIS_LLM", "off")
	t.Setenv("AEGIS_DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	t.Setenv("AEGIS_WORKSPACE", t.TempDir())
	t.Setenv("AEGIS_RULES_RELOAD", "0")
}

// TestMCPServerHTTPRequiresToken: `--http` with no AEGIS_MCP_TOKEN must
// fail fast with a message naming the env var — refusing to serve beats
// exposing the tool registry ungated (ADR-0015).
func TestMCPServerHTTPRequiresToken(t *testing.T) {
	mcpBaseEnv(t)
	var out strings.Builder
	err := mcpServerCmd(context.Background(), []string{"--http", "127.0.0.1:0"}, &out)
	if err == nil {
		t.Fatal("--http without AEGIS_MCP_TOKEN must error")
	}
	if !strings.Contains(err.Error(), "AEGIS_MCP_TOKEN") {
		t.Fatalf("error must name AEGIS_MCP_TOKEN, got: %v", err)
	}
}

// TestMCPServerHTTPFlagParses: the flag parses and the token refusal path
// runs BEFORE any listener binds (a token set would start serving, so we
// only assert the parse+gate order here via the no-token failure above and
// the parse acceptance below).
func TestMCPServerHTTPFlagParses(t *testing.T) {
	mcpBaseEnv(t)
	var out strings.Builder
	// --help exits via flag.ErrHelp, proving the FlagSet owns -http.
	err := mcpServerCmd(context.Background(), []string{"--help"}, &out)
	if err == nil || err.Error() != "flag: help requested" {
		t.Fatalf("want flag.ErrHelp, got %v", err)
	}
	if !strings.Contains(out.String(), "-http") {
		t.Fatalf("usage must document -http, got %q", out.String())
	}
}

// TestMCPServerStdioUnchanged: default invocation (no --http) still builds
// the app and reaches the stdio serve path — pinned by feeding it a closed
// stdin via /dev/null so ServeStdio returns promptly.
func TestMCPServerStdioUnchanged(t *testing.T) {
	mcpBaseEnv(t)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	old := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = old }()

	var out strings.Builder
	// EOF on stdin ends the stdio serve loop; rc 0.
	if err := mcpServerCmd(context.Background(), nil, &out); err != nil {
		t.Fatalf("stdio serve: %v", err)
	}
}
