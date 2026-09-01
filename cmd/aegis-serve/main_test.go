// Tests for the aegis-serve command shell. serve() is driven in-process
// (no signature change was needed — its knobs are all environment), so each
// test boots the real offline pipeline and steers it with AEGIS_ADDR /
// AEGIS_GRPC_ADDR. Listeners bind loopback only; ":99999" fails instantly in
// net.Listen (port out of range) which keeps the listen-error tests
// deterministic — no DNS, no dependence on privileged-port rules.
package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// baseEnv applies the offline baseline: LLM kill switch on, temp store and
// workspace, background loops off, no telegram, no MCP, gRPC disabled (the
// default ":8081" would bind a real shared port). config.Load treats "" like
// unset, so blanking keeps tests hermetic against the developer's shell.
// Every serve test must still set AEGIS_ADDR explicitly — the ":8080"
// default is never wanted in a test.
func baseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AEGIS_MCP_SERVERS", "AEGIS_TELEGRAM_TOKEN", "AEGIS_GRPC_ADDR",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AEGIS_LLM", "off")
	t.Setenv("AEGIS_DB_PATH", t.TempDir()+"/serve.db")
	t.Setenv("AEGIS_WORKSPACE", t.TempDir())
	t.Setenv("AEGIS_RULES_RELOAD", "0")
	t.Setenv("AEGIS_MINER_INTERVAL", "0")
	t.Setenv("AEGIS_GRPC_ADDR", "none")
}

// freeAddr returns a loopback host:port that was free a moment ago. serve()
// does not report the real port behind ":0", so readiness polling needs an
// address the test knows; the grab-and-release race is the standard price
// and is negligible next to the 5s poll deadline.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitHealthy polls GET /healthz until it answers 200, deadline-bounded —
// no fixed sleeps anywhere in this file.
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never answered /healthz", addr)
}

// runServe runs serve in the background and returns a channel carrying its
// return value plus the cancel for the boot context.
func runServe(t *testing.T) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, "smart") }()
	return done, cancel
}

// waitServe asserts serve returned nil within the deadline.
func waitServe(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after context cancel")
	}
}

func TestServeBadTier(t *testing.T) {
	baseEnv(t)
	t.Setenv("AEGIS_ADDR", "127.0.0.1:0")
	err := serve(context.Background(), "bogus")
	if err == nil {
		t.Fatal("serve with bogus tier = nil error, want unknown-tier failure")
	}
	if want := `unknown tier "bogus"`; !strings.Contains(err.Error(), want) {
		t.Errorf("serve error = %q, want it to contain %q", err, want)
	}
}

// TestServeListenError: an out-of-range port makes ListenAndServe fail
// before anything else can go wrong; the error must surface from serve.
func TestServeListenError(t *testing.T) {
	baseEnv(t)
	t.Setenv("AEGIS_ADDR", ":99999")
	err := serve(context.Background(), "smart")
	if err == nil {
		t.Fatal("serve with invalid AEGIS_ADDR = nil error, want the listen error")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("serve error = %q, want it to name the bad port", err)
	}
}

// TestServeGRPCListenError: a valid HTTP listener but an invalid gRPC
// address must abort startup with the gRPC listen error (the HTTP goroutine
// never got a shutdown signal; it stops with the process, as on any boot
// failure in production).
func TestServeGRPCListenError(t *testing.T) {
	baseEnv(t)
	t.Setenv("AEGIS_ADDR", "127.0.0.1:0")
	t.Setenv("AEGIS_GRPC_ADDR", ":99999")
	err := serve(context.Background(), "smart")
	if err == nil {
		t.Fatal("serve with invalid AEGIS_GRPC_ADDR = nil error, want the listen error")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("serve error = %q, want it to name the bad gRPC port", err)
	}
}

// TestServeGRPCDisabled: AEGIS_GRPC_ADDR=none boots HTTP only; cancelling
// the boot context after /healthz is live shuts down cleanly.
func TestServeGRPCDisabled(t *testing.T) {
	baseEnv(t)
	addr := freeAddr(t)
	t.Setenv("AEGIS_ADDR", addr)

	done, cancel := runServe(t)
	waitHealthy(t, addr)
	cancel()
	waitServe(t, done)
}

// TestServeGracefulShutdown: both listeners up (gRPC on an ephemeral port),
// then cancel → HTTP drains via Shutdown, gRPC via GracefulStop, serve
// returns nil.
func TestServeGracefulShutdown(t *testing.T) {
	baseEnv(t)
	addr := freeAddr(t)
	t.Setenv("AEGIS_ADDR", addr)
	t.Setenv("AEGIS_GRPC_ADDR", "127.0.0.1:0")

	done, cancel := runServe(t)
	waitHealthy(t, addr)
	cancel()
	waitServe(t, done)
}
