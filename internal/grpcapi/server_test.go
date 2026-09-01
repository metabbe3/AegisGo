package grpcapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"aegisgo/internal/engine"
	pb "aegisgo/internal/pb"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// fakeEngine: "/"-prefixed prompts are router hits, "err:"-prefixed ones are
// engine failures (decision_source=error); records its trace id.
type fakeEngine struct {
	lastTrace string
}

func (f *fakeEngine) Run(ctx context.Context, prompt string) engine.Result {
	f.lastTrace = trace.From(ctx)
	if strings.HasPrefix(prompt, "err:") {
		return engine.Result{Answer: "engine exploded", DecisionSource: store.SourceError,
			TraceID: f.lastTrace, LatencyMS: 1}
	}
	if strings.HasPrefix(prompt, "/") {
		return engine.Result{Answer: "router answer", DecisionSource: store.SourceRouter,
			RuleID: "uptime", TraceID: f.lastTrace, LatencyMS: 3}
	}
	return engine.Result{Answer: "llm answer", DecisionSource: store.SourceLLM,
		TraceID: f.lastTrace, LatencyMS: 900}
}

// fakeAnswers wraps a real AnswerStore with injectable failures; the
// completed channel records every CompleteAnswer call so tests can observe
// the server's background goroutine through a channel instead of racing on
// shared fields.
type fakeAnswers struct {
	inner       AnswerStore
	putErr      error
	completeErr error
	getErr      error
	completed   chan string
}

func newFakeAnswers(inner AnswerStore) *fakeAnswers {
	return &fakeAnswers{inner: inner, completed: make(chan string, 8)}
}

func (f *fakeAnswers) PutAnswer(ctx context.Context, traceID string) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.inner.PutAnswer(ctx, traceID)
}

func (f *fakeAnswers) CompleteAnswer(ctx context.Context, traceID, statusName, output string) error {
	select {
	case f.completed <- traceID:
	default:
	}
	if f.completeErr != nil {
		return f.completeErr
	}
	return f.inner.CompleteAnswer(ctx, traceID, statusName, output)
}

func (f *fakeAnswers) GetAnswer(ctx context.Context, traceID string) (store.Answer, bool, error) {
	if f.getErr != nil {
		return store.Answer{}, false, f.getErr
	}
	return f.inner.GetAnswer(ctx, traceID)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent, so tests that close the store mid-run need no special case.
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newClient(t *testing.T, e Engine, st *store.Store) pb.AgentClient {
	t.Helper()
	return newCustomClient(t, New(e, st, st, nil))
}

// newCustomClient serves a hand-built Server over an in-memory connection,
// for tests that need nil readiness, a wrapped answer store, or a quiet
// logger.
func newCustomClient(t *testing.T, srv *Server) pb.AgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	Register(s, srv)
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

// pollAnswer polls GetAnswer until want returns true, failing the test at
// the deadline. Completion is asynchronous, so the poll is the only
// synchronization point — no fixed sleeps.
func pollAnswer(ctx context.Context, t *testing.T, c pb.AgentClient, traceID string, want func(*pb.AnswerStatus) bool) *pb.AnswerStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last *pb.AnswerStatus
	var lastErr error
	for {
		a, err := c.GetAnswer(ctx, &pb.GetAnswerRequest{TraceId: traceID})
		if err == nil {
			last = a
			if want(a) {
				return a
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("answer for %s never reached target state: last=%+v err=%v", traceID, last, lastErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// quietLogger keeps the server's error log out of test output for the
// failure-injection cases that are expected to log.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunSyncRouterHit(t *testing.T) {
	st := openStore(t)

	e := &fakeEngine{}
	c := newClient(t, e, st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime", TraceId: "grpc-trace-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DecisionSource != store.SourceRouter || resp.RuleId != "uptime" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.TraceId != "grpc-trace-1" || e.lastTrace != "grpc-trace-1" {
		t.Errorf("trace = %q (engine saw %q)", resp.TraceId, e.lastTrace)
	}
}

// TestServeOnRealListener covers Serve itself: a real loopback listener, a
// real (non-bufconn) client connection, and Serve's return path. Closing the
// listener is the only handle on the grpc.Server Serve builds internally;
// Accept then fails and Serve surfaces that error — nil is reserved for
// Stop/GracefulStop, which never fire on this path.
func TestServeOnRealListener(t *testing.T) {
	st := openStore(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(lis, New(&fakeEngine{}, st, st, nil)) }()

	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := pb.NewAgentClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Run(ctx, &pb.RunRequest{Prompt: "/uptime", TraceId: "tcp-trace-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output != "router answer" || resp.DecisionSource != store.SourceRouter || resp.TraceId != "tcp-trace-1" {
		t.Errorf("resp = %+v", resp)
	}

	if err := lis.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve after listener close = %v, want accept error wrapping net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}

func TestRunEmptyPrompt(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	if _, err := c.Run(context.Background(), &pb.RunRequest{Prompt: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty prompt code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRunAsyncEmptyPrompt(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	if _, err := c.RunAsync(context.Background(), &pb.RunRequest{Prompt: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("RunAsync empty prompt code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGetAnswerEmptyTrace(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	if _, err := c.GetAnswer(context.Background(), &pb.GetAnswerRequest{TraceId: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty trace code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRunAsyncLifecycle(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := c.RunAsync(ctx, &pb.RunRequest{Prompt: "tell me a story"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.TraceId == "" || ack.Status != store.AnswerPending {
		t.Fatalf("ack = %+v", ack)
	}
	a := pollAnswer(ctx, t, c, ack.TraceId, func(a *pb.AnswerStatus) bool { return a.Status == store.AnswerDone })
	if a.Output != "llm answer" {
		t.Errorf("output = %q, want %q", a.Output, "llm answer")
	}
	if a.TraceId != ack.TraceId {
		t.Errorf("answer trace = %q, want ack trace %q", a.TraceId, ack.TraceId)
	}
}

func TestRunAsyncErrorEngine(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := c.RunAsync(ctx, &pb.RunRequest{Prompt: "err: boom"})
	if err != nil {
		t.Fatal(err)
	}
	// decision_source=error must land the answer in the error status with
	// the engine's output attached, not "done".
	a := pollAnswer(ctx, t, c, ack.TraceId, func(a *pb.AnswerStatus) bool { return a.Status == store.AnswerError })
	if a.Output != "engine exploded" {
		t.Errorf("output = %q, want %q", a.Output, "engine exploded")
	}
}

func TestRunAsyncPutAnswerFails(t *testing.T) {
	t.Run("queue", func(t *testing.T) {
		st := openStore(t)
		fa := newFakeAnswers(st)
		fa.putErr = errors.New("answers table locked")
		c := newCustomClient(t, New(&fakeEngine{}, fa, st, quietLogger()))

		_, err := c.RunAsync(context.Background(), &pb.RunRequest{Prompt: "story"})
		if status.Code(err) != codes.Internal {
			t.Errorf("RunAsync code = %v, want Internal", status.Code(err))
		}
	})
	t.Run("complete", func(t *testing.T) {
		st := openStore(t)
		fa := newFakeAnswers(st)
		fa.completeErr = errors.New("disk full")
		c := newCustomClient(t, New(&fakeEngine{}, fa, st, quietLogger()))

		ack, err := c.RunAsync(context.Background(), &pb.RunRequest{Prompt: "story"})
		if err != nil {
			t.Fatal(err)
		}
		// Completion runs in a goroutine the RPC never sees; the fake's
		// channel is the deterministic observation point. Its failure is
		// logged server-side and the stored row must stay pending.
		select {
		case got := <-fa.completed:
			if got != ack.TraceId {
				t.Errorf("completed trace = %q, want %q", got, ack.TraceId)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("background completion never ran")
		}
		a, err := c.GetAnswer(context.Background(), &pb.GetAnswerRequest{TraceId: ack.TraceId})
		if err != nil || a.Status != store.AnswerPending {
			t.Errorf("answer after failed completion = %+v err=%v, want pending", a, err)
		}
	})
}

func TestGetAnswerUnknownTrace(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	if _, err := c.GetAnswer(context.Background(), &pb.GetAnswerRequest{TraceId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown trace code = %v, want NotFound", status.Code(err))
	}
}

func TestGetAnswerStoreError(t *testing.T) {
	st := openStore(t)
	c := newCustomClient(t, New(&fakeEngine{}, st, st, nil))
	// A closed store makes every read fail; the server must surface that as
	// Internal rather than crash or lie with NotFound.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := c.GetAnswer(context.Background(), &pb.GetAnswerRequest{TraceId: "any"})
	if status.Code(err) != codes.Internal {
		t.Errorf("closed store code = %v, want Internal", status.Code(err))
	}
}

func TestReadyOK(t *testing.T) {
	// Nil readiness: nothing to ping, so the server reports ready.
	c := newCustomClient(t, New(&fakeEngine{}, openStore(t), nil, nil))
	r, err := c.Ready(context.Background(), nil)
	if err != nil || !r.Ready {
		t.Errorf("ready = %+v err = %v, want ready", r, err)
	}
}

func TestReadyStoreDown(t *testing.T) {
	st := openStore(t)
	c := newCustomClient(t, New(&fakeEngine{}, st, st, nil))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Unready is a payload, not an RPC error — the probe must succeed and
	// carry the ping failure in Detail.
	r, err := c.Ready(context.Background(), nil)
	if err != nil {
		t.Fatalf("Ready err = %v, want nil", err)
	}
	if r.Ready {
		t.Error("ready = true over closed store, want false")
	}
	if r.Detail == "" {
		t.Error("Detail is empty, want the ping error text")
	}
}

func TestReady(t *testing.T) {
	c := newClient(t, &fakeEngine{}, openStore(t))
	r, err := c.Ready(context.Background(), nil)
	if err != nil || !r.Ready {
		t.Errorf("ready = %+v err = %v", r, err)
	}
}
