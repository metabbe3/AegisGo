package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBotAPI is a scripted Bot API endpoint for the real HTTPClient: an
// httptest server that dispatches on the method name (the URL path suffix
// after /bot<token>/ — see HTTPClient.call's URL construction), records
// every request body, and replies from a per-method response queue with a
// valid {"ok":true,...} envelope as fallback. This is the test side of the
// AEGIS_TELEGRAM_API_BASE seam: real client code, loopback server.
type fakeBotAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []botCall
	queue map[string][]botResponse
}

// botCall is one recorded request.
type botCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// botResponse is one scripted reply.
type botResponse struct {
	status int
	body   string
	// truncate claims a Content-Length larger than the body, so the client
	// sees the connection die mid-response.
	truncate bool
}

// newFakeBotAPI starts the server; it shuts down with the test.
func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{t: t, queue: make(map[string][]botResponse)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// defaultEnvelope is what the endpoint answers for each supported method
// when the test has not scripted anything else.
func defaultEnvelope(method string) string {
	switch method {
	case "getMe":
		return `{"ok":true,"result":{"id":1,"username":"fake_bot"}}`
	case "sendMessage":
		return `{"ok":true,"result":{"message_id":4242}}`
	case "getUpdates":
		return `{"ok":true,"result":[]}`
	default: // editMessageText, setWebhook, deleteWebhook
		return `{"ok":true,"result":true}`
	}
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := path[strings.LastIndex(path, "/")+1:]

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The client only ever posts JSON; anything else is a client bug
		// the test must hear about.
		f.t.Errorf("fakeBotAPI: %s request body is not JSON: %v", method, err)
	}

	f.mu.Lock()
	f.calls = append(f.calls, botCall{Method: method, Path: path, Body: body})
	var resp botResponse
	if q := f.queue[method]; len(q) > 0 {
		resp, f.queue[method] = q[0], q[1:]
	}
	f.mu.Unlock()

	if resp.body == "" {
		resp.body = defaultEnvelope(method)
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	if resp.truncate {
		w.Header().Set("Content-Length", "1024")
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

// client builds a real HTTPClient pointed at the fake endpoint.
func (f *fakeBotAPI) client() *HTTPClient { return NewHTTPClient("test-token", f.srv.URL) }

// respond queues the next reply for method; status 0 means 200.
func (f *fakeBotAPI) respond(method string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[method] = append(f.queue[method], botResponse{status: status, body: body})
}

// respondTruncated queues a reply that dies mid-body.
func (f *fakeBotAPI) respondTruncated(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[method] = append(f.queue[method],
		botResponse{body: `{"ok":true,"resul`, truncate: true})
}

// recordedCalls returns the calls made to method, in order.
func (f *fakeBotAPI) recordedCalls(method string) []botCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []botCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// bodies returns just the JSON bodies sent to method, in order.
func (f *fakeBotAPI) bodies(method string) []map[string]any {
	calls := f.recordedCalls(method)
	out := make([]map[string]any, len(calls))
	for i, c := range calls {
		out[i] = c.Body
	}
	return out
}

func TestUserLabelVariants(t *testing.T) {
	var anon *User
	if got := anon.Label(); got != "unknown" {
		t.Errorf("nil user label = %q, want unknown", got)
	}
	if got := (&User{ID: 1, UserName: "nick"}).Label(); got != "@nick" {
		t.Errorf("username label = %q, want @nick", got)
	}
	if got := (&User{ID: 2, FirstName: "Nick"}).Label(); got != "Nick" {
		t.Errorf("first-name label = %q, want Nick", got)
	}
}

// TestNewHTTPClientDefaultBase pins the default-endpoint wiring by inspecting
// the client struct — no request is made, so no dial to api.telegram.org.
func TestNewHTTPClientDefaultBase(t *testing.T) {
	if c := NewHTTPClient("tok", ""); c.base != DefaultAPIBase {
		t.Errorf("empty base = %q, want default %q", c.base, DefaultAPIBase)
	}
	if c := NewHTTPClient("tok", "http://botapi.local"); c.base != "http://botapi.local" {
		t.Errorf("custom base = %q, want it kept", c.base)
	}
}

func TestGetMeParsesUsername(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()

	name, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "fake_bot" {
		t.Errorf("GetMe = %q, want fake_bot", name)
	}
	calls := api.recordedCalls("getMe")
	if len(calls) != 1 {
		t.Fatalf("getMe calls = %d, want 1", len(calls))
	}
	// The URL shape is /bot<token>/<method> — the token rides the path.
	if !strings.HasPrefix(calls[0].Path, "/bottest-token/") || !strings.HasSuffix(calls[0].Path, "/getMe") {
		t.Errorf("getMe path = %q, want /bottest-token/getMe", calls[0].Path)
	}
}

func TestGetMeHTTPError(t *testing.T) {
	api := newFakeBotAPI(t)
	api.respond("getMe", http.StatusInternalServerError, `{"ok":false,"description":"Not Found"}`)
	c := api.client()

	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe on HTTP 500: want error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("err = %v, want status code and response snippet", err)
	}
}

func TestCallNetworkError(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	api.srv.Close() // every dial fails from here; Close is safe to repeat

	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe against dead endpoint: want error")
	}
	if !strings.Contains(err.Error(), "telegram getMe") {
		t.Errorf("err = %v, want the method name in the wrap", err)
	}
}

func TestSendMessagePayload(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	ctx := context.Background()

	id, err := c.SendMessage(ctx, chatOK, "hello world", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id != 4242 {
		t.Errorf("message_id = %d, want 4242", id)
	}
	if _, err := c.SendMessage(ctx, chatOK, "and hello again", 77); err != nil {
		t.Fatal(err)
	}

	bodies := api.bodies("sendMessage")
	if len(bodies) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(bodies))
	}
	if bodies[0]["chat_id"] != float64(chatOK) || bodies[0]["text"] != "hello world" {
		t.Errorf("first send body = %v", bodies[0])
	}
	if bodies[0]["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview = %v, want true", bodies[0]["disable_web_page_preview"])
	}
	// reply_to_message_id is omitempty: absent on a plain send, present
	// only when quoting a concrete message.
	if _, ok := bodies[0]["reply_to_message_id"]; ok {
		t.Error("reply_to_message_id sent with zero replyTo")
	}
	if bodies[1]["reply_to_message_id"] != float64(77) {
		t.Errorf("reply_to_message_id = %v, want 77", bodies[1]["reply_to_message_id"])
	}
}

func TestEditMessageText(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()

	if err := c.EditMessageText(context.Background(), chatOK, 55, "edited body"); err != nil {
		t.Fatal(err)
	}
	bodies := api.bodies("editMessageText")
	if len(bodies) != 1 {
		t.Fatalf("editMessageText calls = %d, want 1", len(bodies))
	}
	if bodies[0]["chat_id"] != float64(chatOK) ||
		bodies[0]["message_id"] != float64(55) ||
		bodies[0]["text"] != "edited body" {
		t.Errorf("edit body = %v", bodies[0])
	}
}

func TestGetUpdatesParams(t *testing.T) {
	api := newFakeBotAPI(t)
	api.respond("getUpdates", 0,
		`{"ok":true,"result":[{"update_id":500,"message":{"message_id":9,`+
			`"chat":{"id":7,"type":"private"},"from":{"id":8,"username":"nick"},"text":"hi"}}]}`)
	c := api.client()
	ctx := context.Background()

	ups, err := c.GetUpdates(ctx, 101, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].UpdateID != 500 {
		t.Fatalf("updates = %+v, want one with update_id 500", ups)
	}
	if ups[0].Message == nil ||
		ups[0].Message.ChatID() != 7 ||
		ups[0].Message.Text != "hi" ||
		ups[0].Message.From.Label() != "@nick" {
		t.Errorf("parsed message = %+v", ups[0].Message)
	}

	body := api.bodies("getUpdates")[0]
	if body["offset"] != float64(101) {
		t.Errorf("offset = %v, want 101", body["offset"])
	}
	if body["timeout"] != float64(25) {
		t.Errorf("timeout = %v, want 25", body["timeout"])
	}
	allowed, _ := body["allowed_updates"].([]any)
	if len(allowed) != 1 || allowed[0] != "message" {
		t.Errorf("allowed_updates = %v, want [message]", body["allowed_updates"])
	}

	// Offset 0 is omitted (omitempty): the server starts from the oldest.
	if _, err := c.GetUpdates(ctx, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.bodies("getUpdates")[1]["offset"]; ok {
		t.Error("offset sent as 0, want omitted")
	}
}

func TestSetWebhookSendsURLAndSecret(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()

	if err := c.SetWebhook(context.Background(), "https://example.com/hook", "topsecret"); err != nil {
		t.Fatal(err)
	}
	bodies := api.bodies("setWebhook")
	if len(bodies) != 1 {
		t.Fatalf("setWebhook calls = %d, want 1", len(bodies))
	}
	if bodies[0]["url"] != "https://example.com/hook" {
		t.Errorf("url = %v", bodies[0]["url"])
	}
	if bodies[0]["secret_token"] != "topsecret" {
		t.Errorf("secret_token = %v", bodies[0]["secret_token"])
	}
}

func TestDeleteWebhook(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()

	if err := c.DeleteWebhook(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := api.recordedCalls("deleteWebhook"); len(calls) != 1 {
		t.Fatalf("deleteWebhook calls = %d, want 1", len(calls))
	}
}

// TestCallBadResultJSON: a 200 whose body is not the expected envelope must
// surface as an unmarshal error, never as a silent zero value.
func TestCallBadResultJSON(t *testing.T) {
	api := newFakeBotAPI(t)
	api.respond("getMe", 0, `{"ok":true,"result":`) // truncated payload
	c := api.client()

	if _, err := c.GetMe(context.Background()); err == nil {
		t.Fatal("GetMe with undecodable result: want error")
	}
}

// TestClientMethodsSurfaceHTTPErrors: the remaining methods' error arms
// report a failing call the same way GetMe does.
func TestClientMethodsSurfaceHTTPErrors(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	ctx := context.Background()

	api.respond("sendMessage", http.StatusInternalServerError, `{"ok":false,"description":"chat not found"}`)
	if _, err := c.SendMessage(ctx, chatOK, "hi", 0); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("SendMessage err = %v, want HTTP 500 surfaced", err)
	}
	api.respond("getUpdates", http.StatusBadGateway, `{"ok":false}`)
	if _, err := c.GetUpdates(ctx, 1, time.Second); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("GetUpdates err = %v, want HTTP 502 surfaced", err)
	}
}

// TestCallTruncatedResponse: a server that dies mid-body surfaces as a read
// error naming the method.
func TestCallTruncatedResponse(t *testing.T) {
	api := newFakeBotAPI(t)
	api.respondTruncated("getMe")
	c := api.client()

	_, err := c.GetMe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "telegram getMe") ||
		!strings.Contains(err.Error(), "reading response") {
		t.Errorf("err = %v, want a read failure wrapping the method", err)
	}
}

func TestSendMessageWithButtonsPayload(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	ctx := context.Background()

	buttons := [][]Button{{
		{Label: "✅ Approve", Data: "apr:2"},
		{Label: "🚫 Deny", Data: "dny:2"},
	}}
	if _, err := c.SendMessageWithButtons(ctx, chatOK, "🔔 Approval needed", buttons); err != nil {
		t.Fatal(err)
	}
	bodies := api.bodies("sendMessage")
	if len(bodies) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(bodies))
	}
	markup, ok := bodies[0]["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("reply_markup missing/wrong: %v", bodies[0]["reply_markup"])
	}
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("inline rows = %d, want 1", len(rows))
	}
	first, _ := rows[0].([]any)
	if len(first) != 2 {
		t.Fatalf("buttons in row = %d, want 2", len(first))
	}
	b0, _ := first[0].(map[string]any)
	if b0["text"] != "✅ Approve" || b0["callback_data"] != "apr:2" {
		t.Errorf("first button = %v", b0)
	}
}

func TestAnswerCallbackQuery(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	if err := c.AnswerCallbackQuery(context.Background(), "cbq1", "✅ Approved"); err != nil {
		t.Fatal(err)
	}
	bodies := api.bodies("answerCallbackQuery")
	if len(bodies) != 1 || bodies[0]["callback_query_id"] != "cbq1" {
		t.Fatalf("answerCallbackQuery bodies = %v", bodies)
	}
}

// LL-008 pin: getUpdates MUST subscribe to callback_query too — button
// presses silently never arrive otherwise. This test fails on the bug.
func TestGetUpdatesSubscribesCallbackQuery(t *testing.T) {
	api := newFakeBotAPI(t)
	c := api.client()
	ctx := context.Background()
	if _, err := c.GetUpdates(ctx, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	bodies := api.bodies("getUpdates")
	if len(bodies) == 0 {
		t.Fatal("no getUpdates call recorded")
	}
	got, _ := bodies[len(bodies)-1]["allowed_updates"].([]any)
	has := func(kind string) bool {
		for _, k := range got {
			if s, ok := k.(string); ok && s == kind {
				return true
			}
		}
		return false
	}
	if !has("message") || !has("callback_query") {
		t.Fatalf("allowed_updates = %v (need message AND callback_query)", got)
	}
}
