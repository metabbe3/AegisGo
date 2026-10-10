package grpcapi

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// agentServiceFull is the fully-qualified method prefix of the Agent
// service. Auth applies to Agent methods only; grpc.health.v1 and gRPC
// reflection stay open so orchestrator probes (the /healthz equivalent)
// never need a token — mirroring the REST surface, where /healthz and
// /readyz are deliberately credential-free.
const agentServiceFull = "/aegisgo.v1.Agent/"

// bearerOK reports whether the request carries the expected constant-time
// Bearer token. An empty token keeps the server open (the LAN/default
// posture — same semantics as AEGIS_HTTP_TOKEN on the REST side).
func bearerOK(ctx context.Context, token string) bool {
	if token == "" {
		return true
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, v := range md.Get("authorization") {
		const prefix = "Bearer "
		if strings.HasPrefix(v, prefix) &&
			subtle.ConstantTimeCompare([]byte(v[len(prefix):]), []byte(token)) == 1 {
			return true
		}
		// Wrong-shape or wrong-value headers count as failed attempts;
		// the loop continues so multiple authorization values cannot
		// smuggle a valid one past the compare.
	}
	return false
}

// TokenAuth returns gRPC server interceptors enforcing a constant-time
// Bearer match on every Agent method, mirroring the REST bearerAuth
// middleware. Health and reflection services bypass auth by design
// (probe parity with /healthz).
func TokenAuth(token string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, agentServiceFull) && !bearerOK(ctx, token) {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, agentServiceFull) && !bearerOK(ss.Context(), token) {
			return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return handler(srv, ss)
	}
	return unary, stream
}

// NewServer builds a grpc.Server with the Agent service, standard health,
// reflection, and the optional bearer gate — the one construction path the
// cli wiring and Serve both use, so auth cannot be forgotten on one of them.
func NewServer(srv *Server, token string) *grpc.Server {
	unary, stream := TokenAuth(token)
	s := grpc.NewServer(grpc.ChainUnaryInterceptor(unary), grpc.ChainStreamInterceptor(stream))
	Register(s, srv)
	return s
}
