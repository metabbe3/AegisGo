// Tests for the MCP echo-server example. The handlers are driven directly
// with CallToolRequest values shaped like decoded wire arguments, and
// newServer is exercised through a real in-process mcp-go client (same
// pattern as internal/mcpclient's bridge tests) — no stdio, no subprocess.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// callReq builds a request whose arguments look exactly like what the mcp-go
// server hands a handler after JSON decoding.
func callReq(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// resultText returns the first content block's text; every handler in this
// server answers with a single text block.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("tool result has no content blocks")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("first content block = %T, want mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

// mustCall runs a handler and fails the test on Go-level errors; tool-level
// failures arrive as IsError results, which each test asserts itself.
func mustCall(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error),
	req mcp.CallToolRequest) *mcp.CallToolResult {
	t.Helper()
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return res
}

func TestEchoHandler(t *testing.T) {
	res := mustCall(t, echoHandler, callReq(map[string]any{"message": "hello aegis"}))
	if res.IsError {
		t.Fatalf("echo result is an error: %s", resultText(t, res))
	}
	if got := resultText(t, res); got != "hello aegis" {
		t.Errorf("echo text = %q, want the message back", got)
	}
}

// TestEchoMissingParam: a missing required parameter is a TOOL-level error
// result (IsError), not a Go error — the MCP contract for bad input.
func TestEchoMissingParam(t *testing.T) {
	res := mustCall(t, echoHandler, callReq(map[string]any{}))
	if !res.IsError {
		t.Fatalf("echo with no message = success %q, want an error result", resultText(t, res))
	}
	if want := `required argument "message" not found`; !strings.Contains(resultText(t, res), want) {
		t.Errorf("echo error text = %q, want it to contain %q", resultText(t, res), want)
	}
}

// writeCSV writes a header plus n data rows and returns its path.
func writeCSV(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("name,score\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "row%02d,%d\n", i, i)
	}
	path := filepath.Join(t.TempDir(), "data.csv")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing csv: %v", err)
	}
	return path
}

// TestCSVHeadDefaultsAndRows pins the truncation contract: rows+1 lines
// (header + N), the default being 5, and non-positive rows values falling
// back to the default rather than emptying the output.
func TestCSVHeadDefaultsAndRows(t *testing.T) {
	path := writeCSV(t, 10) // header + 10 rows, more than any limit here

	for _, tc := range []struct {
		name string
		args map[string]any
		want int // expected line count
	}{
		{"default 5 rows", map[string]any{"path": path}, 6},
		{"explicit 2 rows", map[string]any{"path": path, "rows": float64(2)}, 3},
		{"rows 0 falls back to default", map[string]any{"path": path, "rows": float64(0)}, 6},
	} {
		res := mustCall(t, csvHeadHandler, callReq(tc.args))
		if res.IsError {
			t.Fatalf("%s: error result: %s", tc.name, resultText(t, res))
		}
		got := strings.Split(resultText(t, res), "\n")
		if len(got) != tc.want {
			t.Errorf("%s: %d lines, want %d (header + rows):\n%s",
				tc.name, len(got), tc.want, resultText(t, res))
		}
		if !strings.Contains(got[0], "name") {
			t.Errorf("%s: first line %q, want the header record", tc.name, got[0])
		}
	}
}

func TestCSVHeadMissingPath(t *testing.T) {
	res := mustCall(t, csvHeadHandler, callReq(map[string]any{}))
	if !res.IsError {
		t.Fatalf("csv_head with no path = success %q, want an error result", resultText(t, res))
	}
	if want := `required argument "path" not found`; !strings.Contains(resultText(t, res), want) {
		t.Errorf("csv_head error text = %q, want it to contain %q", resultText(t, res), want)
	}
}

func TestCSVHeadMissingFile(t *testing.T) {
	res := mustCall(t, csvHeadHandler, callReq(map[string]any{
		"path": filepath.Join(t.TempDir(), "nope.csv"),
	}))
	if !res.IsError {
		t.Fatalf("csv_head on a missing file = success %q, want an error result", resultText(t, res))
	}
	if want := "opening file"; !strings.Contains(resultText(t, res), want) {
		t.Errorf("csv_head error text = %q, want it to contain %q", resultText(t, res), want)
	}
}

func TestCSVHeadMalformedCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.csv")
	// Unterminated quoted field: encoding/csv must fail the ReadAll.
	if err := os.WriteFile(path, []byte("a,\"unclosed\n"), 0o644); err != nil {
		t.Fatalf("writing csv: %v", err)
	}
	res := mustCall(t, csvHeadHandler, callReq(map[string]any{"path": path}))
	if !res.IsError {
		t.Fatalf("csv_head on malformed csv = success %q, want an error result", resultText(t, res))
	}
	if want := "reading csv"; !strings.Contains(resultText(t, res), want) {
		t.Errorf("csv_head error text = %q, want it to contain %q", resultText(t, res), want)
	}
}

// TestNewServerRegistersBothTools drives the full server over an in-process
// client: initialize → ListTools (both tools, and only those) → an actual
// echo CallTool through the registered handler.
func TestNewServerRegistersBothTools(t *testing.T) {
	c, err := client.NewInProcessClient(newServer())
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "echo-server-test", Version: "0.0.1"}
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
	}
	if !names["echo"] || !names["csv_head"] {
		t.Errorf("registered tools = %v, want echo and csv_head", names)
	}
	if len(res.Tools) != 2 {
		t.Errorf("registered %d tools, want exactly 2", len(res.Tools))
	}

	call := mcp.CallToolRequest{}
	call.Params.Name = "echo"
	call.Params.Arguments = map[string]any{"message": "round trip"}
	callRes, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("echo round trip failed: %+v", callRes)
	}
	if got := resultText(t, callRes); got != "round trip" {
		t.Errorf("echo round trip = %q, want %q", got, "round trip")
	}
}
