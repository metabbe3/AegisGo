package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/store"
	"aegisgo/internal/trace"
)

// fakeEngine answers instantly and counts runs; prompts containing "/"
// route deterministically (like a real router hit).
type fakeEngine struct {
	runs atomic.Int64
}

func (f *fakeEngine) Run(ctx context.Context, prompt string) engine.Result {
	f.runs.Add(1)
	if strings.Contains(prompt, "/") {
		return engine.Result{Answer: "router says hi", DecisionSource: store.SourceRouter, TraceID: trace.From(ctx)}
	}
	return engine.Result{Answer: "llm says hi", DecisionSource: store.SourceLLM, TraceID: trace.From(ctx)}
}

// RunStreaming chunks through the same contract as the real engine.
func (f *fakeEngine) RunStreaming(ctx context.Context, prompt string, emit func(string) error) engine.Result {
	if emit != nil {
		_ = emit("chunk ")
	}
	return f.Run(ctx, prompt)
}

// slowEngine simulates an LLM fallback latency.
type slowEngine struct {
	delay time.Duration
	runs  atomic.Int64
}

func (s *slowEngine) Run(_ context.Context, _ string) engine.Result {
	s.runs.Add(1)
	time.Sleep(s.delay)
	return engine.Result{Answer: "slow answer", DecisionSource: store.SourceLLM}
}

func (s *slowEngine) RunStreaming(ctx context.Context, prompt string, emit func(string) error) engine.Result {
	return s.Run(ctx, prompt)
}

func TestHealthzAndReadyz(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: st, Readines: st}))
	defer srv.Close()

	for path := range map[string]int{"/healthz": 200, "/readyz": 200} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", path, resp.StatusCode)
		}
	}
}

func TestTraceMiddlewareEchoesTrace(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/agent/run", strings.NewReader(`{"prompt":"/uptime"}`))
	req.Header.Set("X-Trace-Id", "my-trace-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Trace-Id"); got != "my-trace-42" {
		t.Errorf("echoed trace = %q", got)
	}
	var body runResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.DecisionSource != store.SourceRouter || body.TraceID != "my-trace-42" {
		t.Errorf("body = %+v", body)
	}
}

func TestRunValidation(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", resp.StatusCode)
	}
}

func TestAsyncRunAndPoll(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	slow := &slowEngine{delay: 50 * time.Millisecond}
	srv := httptest.NewServer(Handler(Deps{Engine: slow, Answers: st}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?async=1", "application/json",
		strings.NewReader(`{"prompt":"tell me a story"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var ack struct {
		TraceID string `json:"trace_id"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.TraceID == "" || ack.Status != store.AnswerPending {
		t.Fatalf("ack = %+v", ack)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		ga, err := http.Get(srv.URL + "/v1/answers/" + ack.TraceID)
		if err != nil {
			t.Fatal(err)
		}
		var a store.Answer
		_ = json.NewDecoder(ga.Body).Decode(&a)
		ga.Body.Close()
		if a.Status == store.AnswerDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("answer never completed: %+v", a)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := slow.runs.Load(); n != 1 {
		t.Errorf("engine runs = %d, want exactly 1", n)
	}

	ga, err := http.Get(srv.URL + "/v1/answers/nope")
	if err != nil {
		t.Fatal(err)
	}
	ga.Body.Close()
	if ga.StatusCode != http.StatusNotFound {
		t.Errorf("unknown trace status = %d, want 404", ga.StatusCode)
	}
}

func TestStreamingSSE(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: st, Stats: st}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?stream=1", "application/json",
		strings.NewReader(`{"prompt":"/uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "event: delta") || !strings.Contains(s, "event: done") {
		t.Errorf("sse body = %q", s)
	}
	if !strings.Contains(s, `"decision_source":"regex_router"`) {
		t.Errorf("done event missing decision_source: %q", s)
	}
}

func TestStatsEndpoint(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Audit(context.Background(), store.AuditEvent{
		TraceID: "t1", Interface: store.IFaceREST, DecisionSource: store.SourceRouter,
		Prompt: "/uptime", LatencyMS: 2,
	})
	st.Audit(context.Background(), store.AuditEvent{
		TraceID: "t2", Interface: store.IFaceCLI, DecisionSource: store.SourceLLM,
		Prompt: "story", LatencyMS: 900,
	})

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: st, Stats: st}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s store.StatsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.TotalRuns != 2 || s.DeflectionRate != 0.5 {
		t.Errorf("stats = %+v", s)
	}
	if s.BySource[store.SourceRouter] != 1 || s.ByInterface[store.IFaceCLI] != 1 {
		t.Errorf("breakdown = %+v", s)
	}
}

type nopAnswers struct{}

func (nopAnswers) PutAnswer(context.Context, string) error                      { return nil }
func (nopAnswers) CompleteAnswer(context.Context, string, string, string) error { return nil }
func (nopAnswers) GetAnswer(context.Context, string) (store.Answer, bool, error) {
	return store.Answer{}, false, nil
}
