// Package mcpserver exposes AegisGo's native tool registry as an MCP server
// (ADR-0014): one agent, every interface. The same tools the router, REST,
// gRPC, and Telegram surfaces call are now callable by ANY MCP client
// (Claude Desktop, other agents, AegisGo itself via internal/mcpclient).
//
// Design: tools arrive as a *tools.Registry (already validated, already
// ordered). Each Tool becomes an MCP tool whose input schema is a single
// passthrough "input" JSON string — the Tool interface takes raw JSON
// (Execute(ctx, json.RawMessage)), and AegisGo's typed inputs are JSON
// objects, so the schema honestly says "one JSON object as a string".
// Handler paths: MCP string → json.RawMessage → Tool.Execute. The tool's
// own unmarshal validates; a bad object returns an MCP error result, not a
// transport error.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"aegisgo/internal/tools"
)

// New builds an MCP server exposing every registry tool under its own name.
// Bounded by the registry itself: the tool set is human-curated (builtin +
// admin rules), so listing is naturally capped (rule #10).
func New(reg *tools.Registry, opts ...server.ServerOption) *server.MCPServer {
	s := server.NewMCPServer("aegisgo", "1.0.0",
		append([]server.ServerOption{server.WithToolCapabilities(false)}, opts...)...)
	for _, t := range reg.All() {
		s.AddTool(mcpTool(t), handlerFor(t))
	}
	return s
}

// mcpTool declares the MCP-side shape of one registry tool.
func mcpTool(t tools.Tool) mcp.Tool {
	return mcp.NewTool(t.Name(),
		mcp.WithDescription(t.Description()+" — input is the tool's JSON argument object as a string"),
		mcp.WithString("input", mcp.Required(),
			mcp.Description("JSON object string; e.g. `{\"command\":\"uptime\"}`")),
	)
}

// handlerFor adapts the MCP call into the native Execute path.
func handlerFor(t tools.Tool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input, err := req.RequireString("input")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var probe json.RawMessage
		if err := json.Unmarshal([]byte(input), &probe); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("input is not valid JSON: %v", err)), nil
		}
		out, err := t.Execute(ctx, probe)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("executing "+t.Name(), err), nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("marshaling result", err), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

// ServeStdio runs the server over stdio — the transport MCP clients
// conventionally launch. Returned error means the transport ended badly.
func ServeStdio(s *server.MCPServer) error {
	return server.ServeStdio(s)
}
