package mcpclient

import (
	"context"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/microsoft/agent-framework-go/tool"
)

// newTestServer builds an in-process MCP server exposing two tools:
// echo (text result) and fail (IsError result). The in-process transport
// exercises the whole bridge — real JSON-RPC, no subprocess, no network.
func newTestServer(t *testing.T) *client.Client {
	t.Helper()
	s := server.NewMCPServer("aegis-test", "0.0.1", server.WithToolCapabilities(false))

	echo := mcp.NewTool("echo",
		mcp.WithDescription("Echo the message back"),
		mcp.WithString("message", mcp.Required(), mcp.Description("Text to echo")),
	)
	s.AddTool(echo, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		msg, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("echo: %s", msg)), nil
	})

	fail := mcp.NewTool("boom/bad name!", mcp.WithDescription("Always fails"))
	s.AddTool(fail, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("intentional failure"), nil
	})

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "bridge-test", Version: "0.0.1"}
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatal(err)
	}
	return c
}

// findTool looks an adapted tool up by name; the MCP server does not
// guarantee registration order in ListTools results.
func findTool(t *testing.T, tools []tool.Tool, name string) tool.FuncTool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl.(tool.FuncTool)
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestAdaptAll(t *testing.T) {
	c := newTestServer(t)
	adapted, err := AdaptAll(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(adapted) != 2 {
		t.Fatalf("adapted %d tools, want 2", len(adapted))
	}

	echo := findTool(t, adapted, "echo")
	if echo.Description() != "Echo the message back" {
		t.Errorf("description = %q", echo.Description())
	}
	if echo.Schema() == nil {
		t.Error("Schema() = nil, want the remote input schema")
	}
	// "boom/bad name!" must normalize to the provider-safe alphabet.
	findTool(t, adapted, "boom-bad-name-")
}

func TestBridgeCall(t *testing.T) {
	c := newTestServer(t)
	adapted, err := AdaptAll(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	got, err := findTool(t, adapted, "echo").Call(context.Background(), `{"message":"hello aegis"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: hello aegis" {
		t.Errorf("Call result = %#v", got)
	}

	// A remote IsError result must surface as a Go error, not silent output.
	if _, err := findTool(t, adapted, "boom-bad-name-").Call(context.Background(), `{}`); err == nil {
		t.Error("expected error from failing remote tool")
	}
}

func TestNormalizeToolName(t *testing.T) {
	cases := map[string]string{
		"echo":        "echo",
		"weird/tool":  "weird-tool",
		"sp ace":      "sp-ace",
		"ok_name.1-2": "ok_name.1-2",
	}
	for in, want := range cases {
		if got := NormalizeToolName(in); got != want {
			t.Errorf("NormalizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
	long := NormalizeToolName(longName(100))
	if len(long) > maxToolNameLen {
		t.Errorf("normalized name length = %d, want <= %d", len(long), maxToolNameLen)
	}
}

func longName(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
