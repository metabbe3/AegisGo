// Package grpcapi exposes the hybrid engine over gRPC for cross-language
// callers (PHP, Python, Java, …). Same engine, same audit contract, same
// async answer store as the REST interface — only the wire differs.
package grpcapi

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"aegisgo/internal/engine"
	pb "aegisgo/internal/pb"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// Engine is the slice of *engine.Engine the gRPC layer needs.
type Engine interface {
	Run(ctx context.Context, prompt string) engine.Result
}

// AnswerStore is the async answer store (same contract as REST).
type AnswerStore interface {
	PutAnswer(ctx context.Context, traceID string) error
	CompleteAnswer(ctx context.Context, traceID, statusName, output string) error
	GetAnswer(ctx context.Context, traceID string) (store.Answer, bool, error)
}

// Readiness pings the backing store.
type Readiness interface {
	Ping(ctx context.Context) error
}

// Server implements pb.AgentServer.
type Server struct {
	pb.UnimplementedAgentServer
	engine   Engine
	answers  AnswerStore
	readiness Readiness
	logger   *slog.Logger
}

// New builds the gRPC server implementation.
func New(e Engine, answers AnswerStore, read Readiness, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{engine: e, answers: answers, readiness: read, logger: logger}
}

// Register wires the Agent service, standard health, and reflection onto a
// freshly created grpc.Server.
func Register(s *grpc.Server, srv *Server) {
	pb.RegisterAgentServer(s, srv)
	hs := health.NewServer()
	hs.SetServingStatus("aegisgo.v1.Agent", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s) // grpcurl et al.
}

// Serve blocks serving gRPC on the listener until it stops.
func Serve(l net.Listener, srv *Server) error {
	s := grpc.NewServer()
	Register(s, srv)
	return s.Serve(l)
}

func (s *Server) Run(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
	if req.GetPrompt() == "" {
		return nil, status.Error(codes.InvalidArgument, "prompt is required")
	}
	_, rctx := trace.New(engine.WithIFace(ctx, store.IFaceGRPC), req.GetTraceId())
	res := s.engine.Run(rctx, req.GetPrompt())
	return &pb.RunResponse{
		Output:         res.Answer,
		DecisionSource: res.DecisionSource,
		TraceId:        res.TraceID,
		RuleId:         res.RuleID,
		LatencyMs:      res.LatencyMS,
	}, nil
}

func (s *Server) RunAsync(ctx context.Context, req *pb.RunRequest) (*pb.AsyncAck, error) {
	if req.GetPrompt() == "" {
		return nil, status.Error(codes.InvalidArgument, "prompt is required")
	}
	traceID, rctx := trace.New(engine.WithIFace(ctx, store.IFaceGRPC), req.GetTraceId())
	if err := s.answers.PutAnswer(rctx, traceID); err != nil {
		return nil, status.Errorf(codes.Internal, "queueing answer: %v", err)
	}
	go func(ctx context.Context) {
		res := s.engine.Run(ctx, req.GetPrompt())
		st := store.AnswerDone
		if res.DecisionSource == store.SourceError {
			st = store.AnswerError
		}
		if err := s.answers.CompleteAnswer(context.Background(), traceID, st, res.Answer); err != nil {
			s.logger.Error("grpc: completing answer", "trace_id", traceID, "error", err)
		}
	}(context.WithoutCancel(rctx)) // outlive the RPC, keep trace values
	return &pb.AsyncAck{TraceId: traceID, Status: store.AnswerPending}, nil
}

func (s *Server) GetAnswer(ctx context.Context, req *pb.GetAnswerRequest) (*pb.AnswerStatus, error) {
	if req.GetTraceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "trace_id is required")
	}
	a, ok, err := s.answers.GetAnswer(ctx, req.GetTraceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no answer for trace %s", req.GetTraceId())
	}
	return &pb.AnswerStatus{TraceId: a.TraceID, Status: a.Status, Output: a.Output}, nil
}

func (s *Server) Ready(ctx context.Context, _ *pb.ReadyRequest) (*pb.ReadyResponse, error) {
	if s.readiness != nil {
		if err := s.readiness.Ping(ctx); err != nil {
			return &pb.ReadyResponse{Ready: false, Detail: err.Error()}, nil
		}
	}
	return &pb.ReadyResponse{Ready: true}, nil
}
