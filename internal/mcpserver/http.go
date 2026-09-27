package mcpserver

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// HTTP gates the MCP streamable-HTTP transport behind a bearer token.
//
// Security model (ADR-0015): stdio is spawned by a local client and needs
// no token; an HTTP listener is reachable by anything on the network, so
// remote serving is OPT-IN and token-gated. An empty token refuses to
// serve remotely at all — the same tools stay available over stdio. The
// compare is constant-time for the token body, and the token never enters
// logs: rejects log nothing but the status code.
type HTTP struct {
	srv    *server.MCPServer
	token  string
	stream *server.StreamableHTTPServer
}

// NewHTTP builds the streamable-HTTP surface for an existing MCP server.
// token == "" means the handler must never be mounted remotely (guard in
// the CLI wiring), so construction still succeeds for tests of the gate
// itself.
func NewHTTP(srv *server.MCPServer, token string) *HTTP {
	return &HTTP{
		srv:   srv,
		token: token,
		stream: server.NewStreamableHTTPServer(srv,
			// Stateless: every request is self-contained JSON-RPC. AegisGo's
			// tools are request-scoped (no server-side session state worth
			// keeping), and stateless mode sidesteps session sweeping and
			// resume bookkeeping entirely.
			server.WithStateLess(true),
		),
	}
}

// ServeHTTP is the mounted endpoint: bearer gate first, then the
// streamable-HTTP transport. Wrong or missing token = 401 before any
// JSON-RPC frame is parsed.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized: AEGIS_MCP_TOKEN required", http.StatusUnauthorized)
		return
	}
	h.stream.ServeHTTP(w, r)
}

// authorized checks "Authorization: Bearer <token>" in constant time.
func (h *HTTP) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(got) <= len(prefix) || !strings.EqualFold(got[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(h.token)) == 1
}

// Start runs the listener until ctx is done, then shuts the transport
// down (bounded by the shutdown grace).
func (h *HTTP) Start(ctx context.Context, addr string) error {
	httpSrv := &http.Server{Addr: addr, Handler: h}
	errc := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.stream.Shutdown(shutdownCtx)
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		return err
	}
}
