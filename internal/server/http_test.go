package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aegisgo/internal/engine"
	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
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
	// Audit writes are batched; flush before reading stats back, or the
	// snapshot can catch the second event mid-commit (counted by one SELECT
	// and missed by the next).
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

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

// staticEngine returns a canned result, for error-source paths.
type staticEngine struct {
	result engine.Result
	runs   atomic.Int64
}

func (s *staticEngine) Run(_ context.Context, _ string) engine.Result {
	s.runs.Add(1)
	return s.result
}

func (s *staticEngine) RunStreaming(ctx context.Context, prompt string, emit func(string) error) engine.Result {
	if emit != nil {
		_ = emit("chunk ")
	}
	return s.Run(ctx, prompt)
}

// errAnswers fails selected answer-store calls; nil error = succeed.
type errAnswers struct {
	putErr      error
	completeErr error
	getErr      error
}

func (e errAnswers) PutAnswer(context.Context, string) error { return e.putErr }
func (e errAnswers) CompleteAnswer(context.Context, string, string, string) error {
	return e.completeErr
}
func (e errAnswers) GetAnswer(context.Context, string) (store.Answer, bool, error) {
	return store.Answer{}, false, e.getErr
}

// recordingWebhook records that the mux routed a request to it.
type recordingWebhook struct {
	hits atomic.Int64
}

func (h *recordingWebhook) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits.Add(1)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// recHandler collects log messages so async handler goroutines can be
// observed by polling instead of sleeping.
type recHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

func (h *recHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}

// failWrites makes every Write fail while keeping the recorder's header,
// WriteHeader, and Flush behavior.
type failWrites struct{ *httptest.ResponseRecorder }

func (failWrites) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

// noFlush hides the http.Flusher interface from the wrapped writer, for
// pinning the streaming-unsupported fallback.
type noFlush struct{ http.ResponseWriter }

// waitFor polls cond until it holds or the deadline passes; replaces fixed
// sleeps so async completion stays deterministic.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWebhookMountMounted(t *testing.T) {
	hook := &recordingWebhook{}
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}, Webhook: hook}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+WebhookPath, "application/json", strings.NewReader(`{"update_id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Errorf("webhook post: status=%d body=%q", resp.StatusCode, body)
	}
	if hits := hook.hits.Load(); hits != 1 {
		t.Errorf("webhook hits = %d, want 1", hits)
	}
	// The trace middleware wraps the mounted handler too.
	if resp.Header.Get("X-Trace-Id") == "" {
		t.Error("webhook response missing X-Trace-Id")
	}
}

func TestWebhookMountAbsent(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+WebhookPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHealthzMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	// Only "GET /healthz" is registered; Go 1.22+ method patterns answer
	// other methods with 405.
	resp, err := http.Post(srv.URL+"/healthz", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRunBadJSONBody(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	cases := []struct {
		query, body, wantErr string
	}{
		{"", `{"prompt":`, "invalid JSON body"},
		{"?stream=1", `{"prompt":`, "invalid JSON body"},
		{"?stream=1", `{}`, "prompt is required"},
	}
	for _, tc := range cases {
		resp, err := http.Post(srv.URL+"/v1/agent/run"+tc.query, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		var eb struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q body=%q status = %d, want 400", tc.query, tc.body, resp.StatusCode)
		}
		if !strings.Contains(eb.Error, tc.wantErr) {
			t.Errorf("%q body=%q error = %q, want substring %q", tc.query, tc.body, eb.Error, tc.wantErr)
		}
	}
}

func TestAsyncCompletionPoll(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	slow := &slowEngine{delay: 40 * time.Millisecond}
	srv := httptest.NewServer(Handler(Deps{Engine: slow, Answers: st}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?async=1", "application/json",
		strings.NewReader(`{"prompt":"write a haiku"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		TraceID string `json:"trace_id"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || ack.TraceID == "" || ack.Status != store.AnswerPending {
		t.Fatalf("ack: status=%d body=%+v", resp.StatusCode, ack)
	}

	// Poll until the background run lands, then check the answer payload.
	var got store.Answer
	waitFor(t, "answer done", func() bool {
		ga, err := http.Get(srv.URL + "/v1/answers/" + ack.TraceID)
		if err != nil {
			t.Fatal(err)
		}
		defer ga.Body.Close()
		if ga.StatusCode != http.StatusOK {
			t.Fatalf("poll status = %d, want 200", ga.StatusCode)
		}
		_ = json.NewDecoder(ga.Body).Decode(&got)
		return got.Status == store.AnswerDone
	})
	if got.Output != "slow answer" {
		t.Errorf("output = %q, want the engine's answer", got.Output)
	}
	if got.TraceID != ack.TraceID {
		t.Errorf("answer trace = %q, ack trace = %q", got.TraceID, ack.TraceID)
	}
}

func TestAsyncErrorRunStatus(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng := &staticEngine{result: engine.Result{Answer: "provider exploded", DecisionSource: store.SourceError}}
	srv := httptest.NewServer(Handler(Deps{Engine: eng, Answers: st}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?async=1", "application/json",
		strings.NewReader(`{"prompt":"anything"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		TraceID string `json:"trace_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// A run whose decision_source is "error" must complete as an error
	// answer, not done.
	var got store.Answer
	waitFor(t, "answer error", func() bool {
		ga, err := http.Get(srv.URL + "/v1/answers/" + ack.TraceID)
		if err != nil {
			t.Fatal(err)
		}
		defer ga.Body.Close()
		_ = json.NewDecoder(ga.Body).Decode(&got)
		return got.Status == store.AnswerError
	})
	if got.Output != "provider exploded" {
		t.Errorf("output = %q, want the engine's answer", got.Output)
	}
}

func TestAsyncPutAnswerError(t *testing.T) {
	eng := &fakeEngine{}
	srv := httptest.NewServer(Handler(Deps{
		Engine:  eng,
		Answers: errAnswers{putErr: errors.New("queue full")},
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?async=1", "application/json",
		strings.NewReader(`{"prompt":"/uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(eb.Error, "queueing answer") {
		t.Errorf("error = %q, want substring %q", eb.Error, "queueing answer")
	}
	// Enqueue failed before the goroutine spawned: no run may start.
	if n := eng.runs.Load(); n != 0 {
		t.Errorf("engine runs = %d, want 0", n)
	}
}

func TestAsyncCompleteAnswerErrorLogs(t *testing.T) {
	logs := &recHandler{}
	srv := httptest.NewServer(Handler(Deps{
		Engine:  &fakeEngine{},
		Answers: errAnswers{completeErr: errors.New("disk full")},
		Logger:  slog.New(logs),
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?async=1", "application/json",
		strings.NewReader(`{"prompt":"/uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// The completion failure is logged server-side; the 202 already went
	// out, so polling the log records is the only observable.
	waitFor(t, "completing answer log", func() bool { return logs.has("completing answer") })
}

func TestSSEStreamShape(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: st}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run?stream=1", "application/json",
		strings.NewReader(`{"prompt":"/uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The handler sets the exact literal; equality (not prefix) pins that no
	// charset or parameter sneaks in behind a lenient check.
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	events := strings.Split(strings.TrimSpace(string(body)), "\n\n")
	if len(events) < 2 {
		t.Fatalf("sse events = %d, want at least one delta and a done: %q", len(events), body)
	}
	deltas := 0
	for _, ev := range events[:len(events)-1] {
		if strings.HasPrefix(ev, "event: delta\ndata: ") {
			deltas++
		}
	}
	if deltas == 0 {
		t.Errorf("no delta events in %q", body)
	}

	last := events[len(events)-1]
	if !strings.HasPrefix(last, "event: done\ndata: ") {
		t.Fatalf("last event = %q, want the done terminator", last)
	}
	var done struct {
		DecisionSource string `json:"decision_source"`
		TraceID        string `json:"trace_id"`
		Chunks         int    `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(last, "event: done\ndata: ")), &done); err != nil {
		t.Fatalf("parsing done event: %v", err)
	}
	if done.DecisionSource != store.SourceRouter {
		t.Errorf("done decision_source = %q, want %q", done.DecisionSource, store.SourceRouter)
	}
	if done.TraceID == "" || done.TraceID != resp.Header.Get("X-Trace-Id") {
		t.Errorf("done trace_id = %q, header = %q", done.TraceID, resp.Header.Get("X-Trace-Id"))
	}
	// The done event's chunk count must equal the delta events on the wire.
	if done.Chunks != deltas {
		t.Errorf("done chunks = %d, delta events = %d", done.Chunks, deltas)
	}
}

func TestStreamUnsupportedWithoutFlusher(t *testing.T) {
	// Real servers always provide a Flusher; hide it behind a wrapper to
	// pin the 500 fallback.
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/run?stream=1",
		strings.NewReader(`{"prompt":"/uptime"}`))
	rec := httptest.NewRecorder()
	runAgentStream(noFlush{rec}, req, Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "streaming unsupported") {
		t.Errorf("body = %q, want streaming unsupported", msg)
	}
}

func TestSyncErrorSourceIs500(t *testing.T) {
	eng := &staticEngine{result: engine.Result{
		Answer: "provider down", DecisionSource: store.SourceError, TraceID: "tr-err-1",
	}}
	srv := httptest.NewServer(Handler(Deps{Engine: eng, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run", "application/json", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body runResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if body.DecisionSource != store.SourceError || body.Output != "provider down" {
		t.Errorf("body = %+v", body)
	}
}

// TestRouterToolErrorKeepsSource pins Hard Rule 6 at the HTTP layer: a rule
// that matches but whose tool fails is still a router decision — the JSON
// response says regex_router (200), not error (500).
func TestRouterToolErrorKeepsSource(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Real tools + seeded rules, workspace = repo root (tests run from
	// internal/server), same wiring the engine tests use.
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	set, err := tools.Builtin(tools.Options{Workspace: filepath.Dir(filepath.Dir(wd))})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(reg, router.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	eng := &engine.Engine{Router: rt, Store: st, IFace: store.IFaceREST}

	srv := httptest.NewServer(Handler(Deps{Engine: eng, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run", "application/json",
		strings.NewReader(`{"prompt":"/csv_head no_such_file_anywhere.csv"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body runResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (router decision, not a 5xx)", resp.StatusCode)
	}
	if body.DecisionSource != store.SourceRouter {
		t.Errorf("decision_source = %q, want %q", body.DecisionSource, store.SourceRouter)
	}
	if !strings.Contains(body.Output, "csv_head") || !strings.Contains(body.Output, "failed") {
		t.Errorf("output = %q, want the tool failure naming the rule", body.Output)
	}
	// Trace joins header and body.
	if body.TraceID == "" || body.TraceID != resp.Header.Get("X-Trace-Id") {
		t.Errorf("trace_id = %q, header = %q", body.TraceID, resp.Header.Get("X-Trace-Id"))
	}
}

func TestStatsError(t *testing.T) {
	// A closed store makes Stats fail; the handler must surface 500.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}, Stats: st}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if eb.Error == "" {
		t.Error("error body is empty, want the store failure")
	}
}

func TestStatsUnavailable(t *testing.T) {
	// No Stats dependency wired: 501, not a nil-deref panic.
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if !strings.Contains(eb.Error, "stats unavailable") {
		t.Errorf("error = %q, want stats unavailable", eb.Error)
	}
}

func TestReadinessStoreDown(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}, Readines: st}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(eb.Error, "store unreachable") {
		t.Errorf("error = %q, want store unreachable", eb.Error)
	}
}

func TestAnswerNotFound(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{Engine: &fakeEngine{}, Answers: nopAnswers{}}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/answers/no-such-trace")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(eb.Error, "no-such-trace") {
		t.Errorf("error = %q, want the trace echoed", eb.Error)
	}
}

func TestAnswerStoreError(t *testing.T) {
	srv := httptest.NewServer(Handler(Deps{
		Engine:  &fakeEngine{},
		Answers: errAnswers{getErr: errors.New("db melted")},
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/answers/abc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(eb.Error, "db melted") {
		t.Errorf("error = %q, want the store failure", eb.Error)
	}
}

func TestAnswerEmptyTrace(t *testing.T) {
	// The mux never matches an empty {trace} segment, so the guard is
	// exercised through the handler function itself.
	r := httptest.NewRequest(http.MethodGet, "/v1/answers/", nil)
	r.SetPathValue("trace", "")
	rec := httptest.NewRecorder()
	getAnswer(rec, r, Deps{Answers: nopAnswers{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "trace is required") {
		t.Errorf("body = %q, want trace is required", msg)
	}
}

func TestWriteJSONWriteError(t *testing.T) {
	// The status is already sent when the encode fails; the handler must
	// swallow the error, not panic.
	rec := httptest.NewRecorder()
	writeJSON(failWrites{rec}, http.StatusOK, map[string]string{"status": "ok"})
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 (written before the failure)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written", rec.Body.String())
	}
}

func TestSSEWriteErrors(t *testing.T) {
	t.Run("marshal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := sseWrite(rec, rec, "delta", map[string]any{"text": make(chan int)}); err == nil {
			t.Error("want a marshal error for an unencodable payload")
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want nothing written on marshal failure", rec.Body.String())
		}
	})
	t.Run("write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		fw := failWrites{rec}
		if err := sseWrite(fw, fw, "delta", map[string]string{"text": "hi"}); err == nil {
			t.Error("want a write error from a dead connection")
		}
	})
}
