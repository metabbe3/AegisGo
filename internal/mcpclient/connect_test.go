package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// refusedURL targets loopback port 1, where nothing can listen without root:
// every dial is refused immediately and offline.
const refusedURL = "http://127.0.0.1:1/mcp"

// shortCtx bounds each connect attempt to a few seconds so a wedged dial
// fails the test instead of hanging it.
func shortCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestConnectNoSpecs(t *testing.T) {
	for name, specs := range map[string][]string{"nil": nil, "empty": {}} {
		tools, release, err := Connect(context.Background(), specs)
		if err != nil {
			t.Fatalf("%s specs: %v", name, err)
		}
		if release == nil {
			t.Fatalf("%s specs: release = nil, want callable no-op", name)
		}
		release()
		if tools != nil {
			t.Errorf("%s specs: tools = %v, want nil", name, tools)
		}
	}
}

func TestDialSpecErrors(t *testing.T) {
	cases := []string{
		"stdio:",            // empty command
		"ftp://example.com", // unsupported scheme
		"nonsense",          // neither stdio: nor http(s)://
	}
	for _, spec := range cases {
		if _, err := dial(context.Background(), spec); err == nil {
			t.Errorf("dial(%q) = nil error, want failure", spec)
		}
	}
}

// TestConnectUnsupportedSpec: a spec that is neither stdio: nor http(s)://
// aborts Connect, which returns no tools and no release fn.
func TestConnectUnsupportedSpec(t *testing.T) {
	ctx, cancel := shortCtx(t)
	defer cancel()

	tools, release, err := Connect(ctx, []string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unsupported spec") {
		t.Fatalf("Connect error = %v, want unsupported-spec failure", err)
	}
	if tools != nil {
		t.Errorf("tools = %v, want nil", tools)
	}
	if release != nil {
		t.Error("release = non-nil func, want nil on error")
	}
}

// TestConnectStdioMissingCommand: a stdio: spec without a command is
// rejected before any process is launched.
func TestConnectStdioMissingCommand(t *testing.T) {
	ctx, cancel := shortCtx(t)
	defer cancel()

	_, _, err := Connect(ctx, []string{"stdio:"})
	if err == nil || !strings.Contains(err.Error(), "stdio spec needs a command") {
		t.Fatalf("Connect error = %v, want needs-a-command failure", err)
	}
}

// TestConnectStdioBadBinary: a stdio: spec pointing at a nonexistent binary
// fails at transport start (exec: not found), not at initialize.
func TestConnectStdioBadBinary(t *testing.T) {
	ctx, cancel := shortCtx(t)
	defer cancel()

	spec := "stdio:/nonexistent/aegis-test-binary-xyz"
	_, _, err := Connect(ctx, []string{spec})
	if err == nil {
		t.Fatalf("Connect to missing binary = nil error, want launch failure")
	}
	if !strings.Contains(err.Error(), spec) {
		t.Errorf("error %q does not name the failing spec", err)
	}
	if !strings.Contains(err.Error(), "failed to start stdio transport") {
		t.Errorf("error %q does not mention the stdio transport launch", err)
	}
}

// TestConnectHTTPRefused: an HTTP spec nobody serves fails during
// initialize with the connection-refused error wrapped underneath.
func TestConnectHTTPRefused(t *testing.T) {
	ctx, cancel := shortCtx(t)
	defer cancel()

	tools, release, err := Connect(ctx, []string{refusedURL})
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("Connect error = %v, want initialize failure", err)
	}
	if tools != nil {
		t.Errorf("tools = %v, want nil", tools)
	}
	if release != nil {
		t.Error("release = non-nil func, want nil on error")
	}
}

// TestConnectBadRemoteToolName: a remote tool whose name normalizes to the
// empty string aborts Connect after a successful dial — AdaptAll refuses to
// adapt it, and Connect must release the client it already opened.
func TestConnectBadRemoteToolName(t *testing.T) {
	s := server.NewMCPServer("aegis-test", "0.0.1", server.WithToolCapabilities(false))
	s.AddTool(mcp.NewTool(""), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	httpSrv := server.NewStreamableHTTPServer(s)
	mux := http.NewServeMux()
	mux.Handle("/mcp", httpSrv)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := shortCtx(t)
	defer cancel()

	_, _, err := Connect(ctx, []string{ts.URL + "/mcp"})
	if err == nil || !strings.Contains(err.Error(), "normalizes to empty") {
		t.Fatalf("Connect error = %v, want normalizes-to-empty failure", err)
	}
}

// TestConnectHTTPSuccess drives Connect end to end over a real streamable
// HTTP MCP server on loopback: dial → initialize → list → adapt → call →
// release. The "report" tool returns structured content so the adapter's
// structured branch is exercised over the wire too.
func TestConnectHTTPSuccess(t *testing.T) {
	s := newTestMCPServer()
	s.AddTool(mcp.NewTool("report", mcp.WithDescription("Structured report")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultStructured(map[string]any{"answer": 42, "unit": "s"}, "answer is 42"), nil
		})
	httpSrv := server.NewStreamableHTTPServer(s)
	mux := http.NewServeMux()
	mux.Handle("/mcp", httpSrv)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := shortCtx(t)
	defer cancel()

	tools, release, err := Connect(ctx, []string{ts.URL + "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 { // echo, normalized fail tool, report
		t.Fatalf("Connect adapted %d tools, want 3", len(tools))
	}

	echo := findTool(t, tools, "echo")
	got, err := echo.Call(ctx, `{"message":"over http"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: over http" {
		t.Errorf("echo over HTTP = %#v, want %q", got, "echo: over http")
	}

	// Structured content returned by the server wins over the text fallback.
	report := findTool(t, tools, "report")
	got, err = report.Call(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("report result = %T, want map from structured content", got)
	}
	if m["answer"] != float64(42) || m["unit"] != "s" {
		t.Errorf("report result = %#v, want {answer:42 unit:s}", m)
	}

	// A cancelled context makes the HTTP call fail client-side, surfacing
	// the transport-error branch of the adapter.
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	if _, err := echo.Call(deadCtx, `{"message":"x"}`); err == nil {
		t.Error("Call with cancelled context = nil error, want transport failure")
	}

	// release must close the client promptly — a hung close would deadlock
	// every shutdown path that holds it.
	done := make(chan struct{})
	go func() {
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release() did not return within 5s")
	}
}
