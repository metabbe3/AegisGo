// Command mcp-echo-server is a minimal example of exposing Go tools as an
// MCP server with mark3labs/mcp-go (stdio transport). Run it from an MCP
// client (Claude Desktop, another agent, or AegisGo itself):
//
//	AEGIS_MCP_SERVERS="stdio:./mcp-echo-server" aegis-agent
//
// It doubles as the reference for the server side of MCP: define tools with
// mcp.NewTool, register handlers with AddTool, serve over stdio.
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("mcp-echo-server", "0.1.0",
		server.WithToolCapabilities(false),
	)

	echo := mcp.NewTool("echo",
		mcp.WithDescription("Echo a message back"),
		mcp.WithString("message", mcp.Required(), mcp.Description("Text to echo")),
	)
	s.AddTool(echo, echoHandler)

	head := mcp.NewTool("csv_head",
		mcp.WithDescription("Return the first N rows of a CSV file"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to a CSV file")),
		mcp.WithNumber("rows", mcp.Description("Row count (default 5)")),
	)
	s.AddTool(head, csvHeadHandler)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-echo-server:", err)
		os.Exit(1)
	}
}

func echoHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msg, err := req.RequireString("message")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(msg), nil
}

func csvHeadHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rows := 5
	if v, ok := req.GetArguments()["rows"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			rows = int(f)
		}
	}

	f, err := os.Open(path) //nolint:gosec // example server; real tools should sandbox paths
	if err != nil {
		return mcp.NewToolResultErrorFromErr("opening file", err), nil
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return mcp.NewToolResultErrorFromErr("reading csv", err), nil
	}
	if len(records) > rows+1 {
		records = records[:rows+1] // header + N rows
	}
	out := ""
	for i, rec := range records {
		if i > 0 {
			out += "\n"
		}
		out += fmt.Sprint(rec)
	}
	return mcp.NewToolResultText(out), nil
}
