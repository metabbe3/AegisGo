package mcpserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// httpHarness mounts the gated transport on an httptest server.
func httpHarness(t *testing.T, token string) (*httptest.Server, *client.Client) {
	t.Helper()
	reg := regWith(t)
	h := NewHTTP(New(reg), token)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := client.NewStreamableHttpClient(ts.URL,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatalf("streamable http client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return ts, c
}

func httpInit(t *testing.T, c *client.Client) {
	t.Helper()
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "mcphttp-test", Version: "0.0.1"}
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

// TestHTTPRoundTrip: with the right bearer token, a real MCP client does
// initialize → ListTools → CallTool through the streamable-HTTP transport.
func TestHTTPRoundTrip(t *testing.T) {
	_, c := httpHarness(t, "sekrit")
	httpInit(t, c)

	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := false
	for _, tl := range res.Tools {
		if tl.Name == "system_command" {
			found = true
		}
	}
	if !found {
		t.Fatal("system_command missing from HTTP tool list")
	}

	call := mcp.CallToolRequest{}
	call.Params.Name = "system_command"
	call.Params.Arguments = map[string]any{"input": `{"command":"hostname"}`}
	out, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("call failed: %+v", out)
	}
	if !strings.Contains(resultText(t, out), "policy_tier") {
		t.Fatalf("result = %q", resultText(t, out))
	}
}

// TestHTTPRejectsWrongToken: a wrong bearer token gets 401 before any
// JSON-RPC frame is parsed — the initialize handshake itself fails.
func TestHTTPRejectsWrongToken(t *testing.T) {
	ts, c := httpHarness(t, "sekrit")
	// Swap in a client with the WRONG token: same endpoint, bad header.
	bad, err := client.NewStreamableHttpClient(ts.URL,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer wrong"}))
	if err != nil {
		t.Fatalf("bad client: %v", err)
	}
	t.Cleanup(func() { _ = bad.Close() })
	_ = c // harness client unused here

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "bad", Version: "0"}
	if _, err := bad.Initialize(context.Background(), initReq); err == nil {
		t.Fatal("initialize with wrong token must fail")
	}

	// And the raw wire contract: 401, not a JSON-RPC error.
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer nope")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHTTPRejectsMissingToken: no Authorization header at all = 401.
func TestHTTPRejectsMissingToken(t *testing.T) {
	ts, _ := httpHarness(t, "sekrit")
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHTTPStartStopsOnContextCancel: Start returns nil shortly after ctx
// cancellation (listener torn down, no goroutine leak into the test).
func TestHTTPStartStopsOnContextCancel(t *testing.T) {
	reg := regWith(t)
	h := NewHTTP(New(reg), "sekrit")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx, "127.0.0.1:0") }()
	// :0 means "pick a port"; ListenAndServe binds it, then we cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}
