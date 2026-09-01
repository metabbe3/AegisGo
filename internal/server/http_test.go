package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
)

// stubRunner returns a canned final message without touching a provider.
type stubRunner struct{}

func (stubRunner) RunText(_ context.Context, msg string, _ ...agent.Option) agent.ResponseStream {
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		// Stand-in final update echoing the prompt as assistant text.
		yield(&agent.ResponseUpdate{
			Role:     message.RoleAssistant,
			Contents: message.Contents{&message.TextContent{Text: msg}},
		}, nil)
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(Handler(stubRunner{}, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["status"] != "ok" {
		t.Errorf("body = %v, %v", body, err)
	}
}

func TestRunEndpoint(t *testing.T) {
	srv := httptest.NewServer(Handler(stubRunner{}, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/agent/run", "application/json",
		strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var body runResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	resp2, err := http.Post(srv.URL+"/v1/agent/run", "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", resp2.StatusCode)
	}
}
