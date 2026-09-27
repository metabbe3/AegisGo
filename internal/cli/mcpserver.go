package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/logx"
	"aegisgo/internal/mcpserver"
	"aegisgo/internal/store"
)

// mcpServerCmd runs `aegis mcp-server`: the native tool registry exposed
// over MCP (ADR-0014). Default transport is stdio — designed to be embedded
// by MCP clients (Claude Desktop, other agents) the same way
// examples/mcp-echo-server is, but with the REAL tool set.
//
// `--http <addr>` switches to the streamable-HTTP transport (ADR-0015):
// remote-capable and therefore REQUIRES AEGIS_MCP_TOKEN — an empty token
// refuses to serve rather than exposing the tool registry ungated.
func mcpServerCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	httpAddr := fs.String("http", "", "serve streamable-HTTP MCP on this addr instead of stdio (requires AEGIS_MCP_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	logger := logx.New(cfg.LogLevel, stderr, false)
	a, cleanup, err := app.Build(ctx, cfg, config.TierSmart, store.IFaceREST, logger)
	if err != nil {
		return fmt.Errorf("mcp-server: %w", err)
	}
	defer cleanup()
	if a.Registry == nil {
		return fmt.Errorf("mcp-server: tool registry unavailable")
	}

	if *httpAddr == "" {
		return mcpserver.ServeStdio(mcpserver.New(a.Registry))
	}
	if cfg.MCPToken == "" {
		return fmt.Errorf("mcp-server: --http requires AEGIS_MCP_TOKEN (refusing to expose the tool registry without a bearer gate)")
	}
	logger.Info("mcp-server: streamable HTTP listening", "addr", *httpAddr)
	return mcpserver.NewHTTP(mcpserver.New(a.Registry), cfg.MCPToken).Start(ctx, *httpAddr)
}
