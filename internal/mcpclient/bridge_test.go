package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/microsoft/agent-framework-go/tool"
)

// newTestMCPServer builds an MCP server exposing two tools: echo (text
// result) and fail (IsError result). Shared by the in-process bridge tests
// and the HTTP transport test in connect_test.go so both exercise the same
// tool set.
func newTestMCPServer() *server.MCPServer {
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

	return s
}

// newTestServer wraps newTestMCPServer in an in-process client. The
// in-process transport exercises the whole bridge — real JSON-RPC, no
// subprocess, no network.
func newTestServer(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.NewInProcessClient(newTestMCPServer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	initClient(t, c)
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

// initClient initializes a raw client the same way newTestServer does, for
// servers built ad hoc (collision tests, HTTP transport tests).
func initClient(t *testing.T, c *client.Client) {
	t.Helper()
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "bridge-test", Version: "0.0.1"}
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatal(err)
	}
}

// TestReturnSchemaVariants pins the three ReturnSchema shapes: raw output
// schema verbatim, structured output schema by value, nil when absent. The
// adapter is constructed directly — these methods read only the tool
// definition, so no client is needed.
func TestReturnSchemaVariants(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"out":{"type":"string"}}}`)
	a := &mcpToolAdapter{def: mcp.Tool{Name: "raw-out", RawOutputSchema: raw}}
	got, ok := a.ReturnSchema().(json.RawMessage)
	if !ok {
		t.Fatalf("ReturnSchema() = %T, want json.RawMessage", a.ReturnSchema())
	}
	if string(got) != string(raw) {
		t.Errorf("ReturnSchema() = %s, want %s", got, raw)
	}

	structured := &mcpToolAdapter{def: mcp.Tool{
		Name: "typed-out",
		OutputSchema: mcp.ToolOutputSchema{
			Type:       "object",
			Properties: map[string]any{"out": map[string]any{"type": "string"}},
			Required:   []string{"out"},
		},
	}}
	sch, ok := structured.ReturnSchema().(mcp.ToolOutputSchema)
	if !ok {
		t.Fatalf("ReturnSchema() = %T, want mcp.ToolOutputSchema", structured.ReturnSchema())
	}
	if sch.Type != "object" {
		t.Errorf("OutputSchema.Type = %q, want object", sch.Type)
	}
	prop, ok := sch.Properties["out"].(map[string]any)
	if !ok || prop["type"] != "string" {
		t.Errorf("OutputSchema.Properties[out] = %#v, want {type: string}", sch.Properties["out"])
	}
	if len(sch.Required) != 1 || sch.Required[0] != "out" {
		t.Errorf("OutputSchema.Required = %#v, want [out]", sch.Required)
	}

	if got := (&mcpToolAdapter{def: mcp.Tool{Name: "bare"}}).ReturnSchema(); got != nil {
		t.Errorf("ReturnSchema() with no output schema = %#v, want nil", got)
	}
}

// TestSchemaRawInputSchema pins the raw-input-schema branch of Schema: the
// server-declared JSON is handed to the framework verbatim, not re-typed.
func TestSchemaRawInputSchema(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"q":{"type":"number"}},"required":["q"]}`)
	a := &mcpToolAdapter{def: mcp.Tool{Name: "raw-in", RawInputSchema: raw}}
	got, ok := a.Schema().(json.RawMessage)
	if !ok {
		t.Fatalf("Schema() = %T, want json.RawMessage", a.Schema())
	}
	if string(got) != string(raw) {
		t.Errorf("Schema() = %s, want %s", got, raw)
	}
}

// TestContentTextFlattensMixedBlocks covers every content-block branch of
// contentText: text, image, audio, and the placeholder fallback for blocks
// the bridge does not decode (EmbeddedResource).
func TestContentTextFlattensMixedBlocks(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{
		mcp.TextContent{Type: "text", Text: "hello"},
		mcp.ImageContent{Type: "image", Data: "aGk=", MIMEType: "image/png"},
		mcp.AudioContent{Type: "audio", Data: "aGk=", MIMEType: "audio/wav"},
		mcp.EmbeddedResource{},
	}}
	want := "hello\n[image content: image/png]\n[audio content: audio/wav]\n[mcp.EmbeddedResource]"
	if got := contentText(res); got != want {
		t.Errorf("contentText() = %q, want %q", got, want)
	}
}

// TestBridgeCallArgumentDecoding covers Call's argument handling: malformed
// JSON fails before any RPC is sent, while blank and "null" arguments skip
// decoding and reach the server as an empty object (echo then rejects the
// missing required parameter server-side).
func TestBridgeCallArgumentDecoding(t *testing.T) {
	c := newTestServer(t)
	adapted, err := AdaptAll(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	echo := findTool(t, adapted, "echo")

	if _, err := echo.Call(context.Background(), `{"message":`); err == nil || !strings.Contains(err.Error(), "decoding arguments") {
		t.Errorf("Call with malformed JSON error = %v, want decoding-arguments failure", err)
	}
	for _, args := range []string{"", "   ", "null"} {
		if _, err := echo.Call(context.Background(), args); err == nil {
			t.Errorf("Call(%q) = nil error, want missing-required-parameter failure", args)
		}
	}
}

// TestAdaptToolEmptyName: a tool whose name normalizes to the empty string
// must be rejected instead of adapted under an empty provider name.
func TestAdaptToolEmptyName(t *testing.T) {
	if _, err := AdaptTool(nil, mcp.Tool{Name: ""}); err == nil {
		t.Fatal("AdaptTool with empty name = nil error, want rejection")
	}
}

// TestAdaptAllNameCollision: two remote names that normalize to the same
// provider-safe name must abort adaptation rather than shadow each other.
func TestAdaptAllNameCollision(t *testing.T) {
	s := server.NewMCPServer("aegis-test", "0.0.1", server.WithToolCapabilities(false))
	for _, name := range []string{"dup/a", "dup-a"} {
		s.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	initClient(t, c)

	if _, err := AdaptAll(context.Background(), c); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("AdaptAll collision error = %v, want name-collision failure", err)
	}
}

// TestAdaptAllListToolsFailure: a client whose transport cannot reach a
// server must surface the listing error instead of an empty tool set.
func TestAdaptAllListToolsFailure(t *testing.T) {
	// Loopback port 1 has no listener: the un-initialized streamable HTTP
	// client fails its ListTools POST immediately.
	cli, err := client.NewStreamableHttpClient(refusedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AdaptAll(ctx, cli); err == nil || !strings.Contains(err.Error(), "listing mcp tools") {
		t.Fatalf("AdaptAll error = %v, want listing failure", err)
	}
}
