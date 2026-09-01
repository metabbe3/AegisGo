package telegram

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
)

// WebhookHeader is the header Telegram signs every delivery with; the value
// equals the secret_token given to setWebhook. The handler mounts at
// server.WebhookPath in aegis-serve.
const WebhookHeader = "X-Telegram-Bot-Api-Secret-Token"

// NewWebhookHandler builds the webhook transport: a thin shim that checks
// the shared secret, enqueues the update, and 200s immediately. The engine
// runs in the worker pool — never in the HTTP handler. Telegram retries
// deliveries that are slow or non-2xx, so the ack must never wait on
// processing (that is how duplicate LLM spend and duplicate replies happen).
func NewWebhookHandler(secret string, inbox *Inbox, pool *WorkerPool, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Constant-time compare: the secret is the only thing standing
		// between the open internet and the agent's inbox.
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(WebhookHeader)), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusUnauthorized)
			return
		}

		var u Update
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&u); err != nil {
			// Still 200: anything else makes Telegram retry the poison
			// payload forever. Log and drop instead.
			logger.Error("telegram: undecodable webhook payload dropped", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		if u.UpdateID == 0 {
			logger.Warn("telegram: webhook payload without update_id dropped")
			w.WriteHeader(http.StatusOK)
			return
		}

		known, err := inbox.Enqueue(r.Context(), u)
		if err != nil {
			// Enqueue failed: 500 so Telegram redelivers (at-least-once).
			http.Error(w, "inbox unavailable", http.StatusInternalServerError)
			return
		}
		if !known {
			logger.Debug("telegram: duplicate delivery ignored", "update_id", u.UpdateID)
		}
		pool.Wake()
		w.WriteHeader(http.StatusOK)
	})
}
