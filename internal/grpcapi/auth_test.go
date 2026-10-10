package grpcapi

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "aegisgo/internal/pb"
	"aegisgo/internal/store"
)

// newAuthClient serves a Server behind a bearer token over bufconn and
// returns a plain client — tests attach (or omit) credentials per call.
func newAuthClient(t *testing.T, srv *Server, token string) pb.AgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := NewServer(srv, token)
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewAgentClient(conn)
}

func netConnDialer(lis *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) { return lis.Dial() }
}

// TestTokenAuthRejectsMissingToken pins the security contract: with
// AEGIS_GRPC_TOKEN set, an Agent call without credentials fails
// Unauthenticated and never reaches the engine.
func TestTokenAuthRejectsMissingToken(t *testing.T) {
	st := openStore(t)
	e := &fakeEngine{}
	c := newAuthClient(t, New(e, st, st, nil, nil), "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
	if e.lastTrace != "" {
		t.Fatalf("engine saw the rejected call (trace %q)", e.lastTrace)
	}
}

// TestTokenAuthRejectsWrongToken: a well-shaped Bearer header with the
// wrong value is still refused.
func TestTokenAuthRejectsWrongToken(t *testing.T) {
	st := openStore(t)
	c := newAuthClient(t, New(&fakeEngine{}, st, st, nil, nil), "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer wrong")

	if _, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}

// TestTokenAuthAcceptsValidToken: the right Bearer reaches the engine.
func TestTokenAuthAcceptsValidToken(t *testing.T) {
	st := openStore(t)
	e := &fakeEngine{}
	c := newAuthClient(t, New(e, st, st, nil, nil), "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer s3cret")

	resp, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DecisionSource != store.SourceRouter {
		t.Errorf("resp = %+v", resp)
	}
}

// TestTokenAuthOpenWhenEmpty: empty token keeps the legacy open posture
// (LAN default) — a no-credential call succeeds.
func TestTokenAuthOpenWhenEmpty(t *testing.T) {
	st := openStore(t)
	c := newAuthClient(t, New(&fakeEngine{}, st, st, nil, nil), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime"}); err != nil {
		t.Fatalf("open posture broken: %v", err)
	}
}

// TestTokenAuthHealthStaysOpen: health is the /healthz equivalent — it
// must answer without a token even when auth is on, so orchestrator
// probes keep working.
func TestTokenAuthHealthStaysOpen(t *testing.T) {
	st := openStore(t)
	lis := bufconn.Listen(1 << 20)
	s := NewServer(New(&fakeEngine{}, st, st, nil, nil), "s3cret")
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(netConnDialer(lis)),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health probe behind token: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("status = %v", resp.Status)
	}
}
