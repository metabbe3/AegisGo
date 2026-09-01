// Package telegram implements AegisGo's Telegram interface: a stdlib-only
// Bot API client (no dependency — the methods we need are plain
// JSON-over-HTTPS), a transport-free dispatcher over the hybrid engine, and
// two thin transports (webhook and long-poll) fed by a durable inbox.
//
// Delivery semantics: at-least-once with idempotent replies keyed on
// update_id. Webhooks ack immediately and enqueue; the long-poll offset
// advances only past processed rows (process-then-ack, never the reverse —
// advancing first would destroy messages on a crash).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultAPIBase is the official Bot API endpoint. Overridable for tests
// and self-hosted bot API servers (config AEGIS_TELEGRAM_API_BASE).
const DefaultAPIBase = "https://api.telegram.org"

// Client is the Bot API surface the interface needs. Fakes in tests
// implement it; the real one is a thin HTTPS wrapper.
type Client interface {
	// GetMe validates the token and returns the bot username.
	GetMe(ctx context.Context) (string, error)
	// SendMessage posts text to a chat; returns the new message id.
	SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error)
	// EditMessageText rewrites a previously sent message.
	EditMessageText(ctx context.Context, chatID, messageID int64, text string) error
	// GetUpdates long-polls for updates at or after offset (0 = from the
	// stored high-water mark's next).
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	// SetWebhook registers the webhook URL with the given secret.
	SetWebhook(ctx context.Context, url, secret string) error
	// DeleteWebhook removes the webhook registration.
	DeleteWebhook(ctx context.Context) error
}

// Update is the subset of Telegram's Update the dispatcher handles. Only
// text messages are processed; everything else is ignored cheaply.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message is the subset of Telegram's Message we need. Note the nested
// shapes — this mirrors the wire format exactly (chat.id, from.username).
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	From      *User  `json:"from,omitempty"`
}

// ChatID returns the chat this message belongs to.
func (m *Message) ChatID() int64 { return m.Chat.ID }

// Chat is a Telegram chat (private, group, or channel).
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	UserName  string `json:"username"`
	FirstName string `json:"first_name"`
}

// Label returns the best human handle for logs.
func (u *User) Label() string {
	if u == nil {
		return "unknown"
	}
	if u.UserName != "" {
		return "@" + u.UserName
	}
	return u.FirstName
}

// HTTPClient talks to the Bot API over HTTPS.
type HTTPClient struct {
	token string
	base  string
	http  *http.Client
}

// NewHTTPClient builds a Bot API client. Empty base uses DefaultAPIBase.
func NewHTTPClient(token, base string) *HTTPClient {
	if base == "" {
		base = DefaultAPIBase
	}
	return &HTTPClient{
		token: token,
		base:  base,
		// Long-poll requests hold the connection for `timeout` seconds;
		// the transport timeout must exceed that or every poll errors.
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

// call posts a JSON method and decodes result into out (nil to discard).
func (c *HTTPClient) call(ctx context.Context, method string, in any, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", c.base, c.token, method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: reading response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram %s: HTTP %d: %.200s", method, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *HTTPClient) GetMe(ctx context.Context) (string, error) {
	var resp struct {
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := c.call(ctx, "getMe", struct{}{}, &resp); err != nil {
		return "", err
	}
	return resp.Result.Username, nil
}

func (c *HTTPClient) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	in := struct {
		ChatID         int64  `json:"chat_id"`
		Text           string `json:"text"`
		ReplyToMessage int64  `json:"reply_to_message_id,omitempty"`
		DisableWebPage bool   `json:"disable_web_page_preview"`
	}{ChatID: chatID, Text: text, ReplyToMessage: replyTo, DisableWebPage: true}
	var resp struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := c.call(ctx, "sendMessage", in, &resp); err != nil {
		return 0, err
	}
	return resp.Result.MessageID, nil
}

func (c *HTTPClient) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	in := struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
	}{ChatID: chatID, MessageID: messageID, Text: text}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	return c.call(ctx, "editMessageText", in, &resp)
}

func (c *HTTPClient) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	in := struct {
		Offset         int64    `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{Offset: offset, Timeout: int(timeout.Seconds()), AllowedUpdates: []string{"message"}}
	var resp struct {
		Result []Update `json:"result"`
	}
	if err := c.call(ctx, "getUpdates", in, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func (c *HTTPClient) SetWebhook(ctx context.Context, url, secret string) error {
	in := struct {
		URL            string   `json:"url"`
		SecretToken    string   `json:"secret_token,omitempty"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{URL: url, SecretToken: secret, AllowedUpdates: []string{"message"}}
	return c.call(ctx, "setWebhook", in, nil)
}

func (c *HTTPClient) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", struct{}{}, nil)
}
