package grpcapi

import (
	"context"
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

// fakeEngine: "/"-prefixed prompts are router hits; records its trace id.
type fakeEngine struct {
	lastTrace string
}

func (f *fakeEngine) Run(ctx context.Context, prompt string) engine.Result {
	f.lastTrace = trace.From(ctx)
	if strings.HasPrefix(prompt, "/") {
		return engine.Result{Answer: "router answer", DecisionSource: store.SourceRouter,
			RuleID: "uptime", TraceID: f.lastTrace, LatencyMS: 3}
	}
	return engine.Result{Answer: "llm answer", DecisionSource: store.SourceLLM,
		TraceID: f.lastTrace, LatencyMS: 900}
}

func newClient(t *testing.T, e Engine, st *store.Store) pb.AgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	Register(srv, New(e, st, st, nil))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

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

func TestRunSyncRouterHit(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

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

func TestRunValidation(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := newClient(t, &fakeEngine{}, st)
	if _, err := c.Run(context.Background(), &pb.RunRequest{Prompt: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty prompt code = %v", status.Code(err))
	}
}

func TestRunAsyncAndPoll(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := newClient(t, &fakeEngine{}, st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ack, err := c.RunAsync(ctx, &pb.RunRequest{Prompt: "tell me a story"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.TraceId == "" || ack.Status != store.AnswerPending {
		t.Fatalf("ack = %+v", ack)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		a, err := c.GetAnswer(ctx, &pb.GetAnswerRequest{TraceId: ack.TraceId})
		if err == nil && a.Status == store.AnswerDone {
			if a.Output != "llm answer" {
				t.Errorf("output = %q", a.Output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("answer never completed: %+v %v", a, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := c.GetAnswer(ctx, &pb.GetAnswerRequest{TraceId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown trace code = %v", status.Code(err))
	}
}

func TestReady(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := newClient(t, &fakeEngine{}, st)
	r, err := c.Ready(context.Background(), nil)
	if err != nil || !r.Ready {
		t.Errorf("ready = %+v err = %v", r, err)
	}
}
