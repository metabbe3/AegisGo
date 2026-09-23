package cli

import (
	"context"
	"fmt"
	"io"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/logx"
	"aegisgo/internal/mcpserver"
	"aegisgo/internal/store"
)

// mcpServerCmd runs `aegis mcp-server`: the native tool registry exposed
// over MCP stdio (ADR-0014). Designed to be embedded by MCP clients
// (Claude Desktop, other agents) the same way examples/mcp-echo-server is,
// but with the REAL tool set — no echo toys.
func mcpServerCmd(ctx context.Context, _ []string, stderr io.Writer) error {
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
	return mcpserver.ServeStdio(mcpserver.New(a.Registry))
}
