// Package mcpclient connects an AegisGo agent to external MCP servers using
// mark3labs/mcp-go, and adapts each remote tool to the agent-framework-go
// tool.FuncTool interface so the agent loop can call it like any builtin.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/microsoft/agent-framework-go/tool"
)

// maxToolNameLen is the OpenAI function-name limit; names are also restricted
// to [A-Za-z0-9_.-] by several providers, so remote MCP names are normalized
// the same way the framework's own mcptool bridge does it.
const maxToolNameLen = 64

// AdaptTool wraps one remote MCP tool as an agent-framework FuncTool. Calls
// are forwarded verbatim (arguments in, content out) — the agent never knows
// the tool lives in another process.
func AdaptTool(cli client.MCPClient, def mcp.Tool) (tool.FuncTool, error) {
	name := NormalizeToolName(def.Name)
	if name == "" {
		return nil, fmt.Errorf("mcp tool name %q normalizes to empty", def.Name)
	}
	return &mcpToolAdapter{cli: cli, def: def, name: name}, nil
}

// AdaptAll wraps every tool the client can list.
func AdaptAll(ctx context.Context, cli client.MCPClient) ([]tool.Tool, error) {
	list, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing mcp tools: %w", err)
	}
	seen := make(map[string]bool, len(list.Tools))
	var out []tool.Tool
	for _, def := range list.Tools {
		adapted, err := AdaptTool(cli, def)
		if err != nil {
			return nil, err
		}
		if seen[adapted.Name()] {
			return nil, fmt.Errorf("mcp tool name collision after normalization: %q", adapted.Name())
		}
		seen[adapted.Name()] = true
		out = append(out, adapted)
	}
	return out, nil
}

type mcpToolAdapter struct {
	cli  client.MCPClient
	def  mcp.Tool
	name string // provider-safe name; def.Name stays the wire name
}

func (a *mcpToolAdapter) Name() string        { return a.name }
func (a *mcpToolAdapter) Description() string { return a.def.Description }

// Schema returns the remote tool's JSON Schema so the framework can advertise
// the tool to the LLM exactly as the MCP server declared it.
func (a *mcpToolAdapter) Schema() any {
	if len(a.def.RawInputSchema) > 0 {
		return json.RawMessage(a.def.RawInputSchema)
	}
	return a.def.InputSchema
}

func (a *mcpToolAdapter) ReturnSchema() any {
	if len(a.def.RawOutputSchema) > 0 {
		return json.RawMessage(a.def.RawOutputSchema)
	}
	if a.def.OutputSchema.Type != "" {
		return a.def.OutputSchema
	}
	return nil
}

func (a *mcpToolAdapter) Call(ctx context.Context, args string) (any, error) {
	arguments := map[string]any{}
	if s := strings.TrimSpace(args); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &arguments); err != nil {
			return nil, fmt.Errorf("decoding arguments for %s: %w", a.name, err)
		}
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = a.def.Name // wire name, not the normalized one
	req.Params.Arguments = arguments

	res, err := a.cli.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("calling mcp tool %s: %w", a.name, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp tool %s failed: %s", a.name, contentText(res))
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	return contentText(res), nil
}

// contentText flattens a CallToolResult's content blocks into plain text.
func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for i, c := range res.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch v := c.(type) {
		case mcp.TextContent:
			b.WriteString(v.Text)
		case mcp.ImageContent:
			fmt.Fprintf(&b, "[image content: %s]", v.MIMEType)
		case mcp.AudioContent:
			fmt.Fprintf(&b, "[audio content: %s]", v.MIMEType)
		default:
			// EmbeddedResource and future block kinds degrade to a
			// placeholder; agents needing them should use structured
			// content instead.
			fmt.Fprintf(&b, "[%T]", c)
		}
	}
	return b.String()
}

// NormalizeToolName maps an MCP tool name onto the [A-Za-z0-9_.-] alphabet
// providers require, truncating to maxToolNameLen.
func NormalizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= maxToolNameLen {
			break
		}
	}
	return b.String()
}
