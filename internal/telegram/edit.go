package telegram

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// Decision editor (ADR-0009): when an approval is decided, the pushed
// message with its ✅/🚫 buttons is EDITED to a final state line. The
// chat stays a tidy record — "decided" instead of a growing pile of
// replies — and, critically, the buttons disappear so nobody can press
// a stale button hours later (presses on a decided row are already
// honest no-ops; this makes the UI agree with the ledger).
//
// Mapping: the notifier records (approvalID → chatID, messageID) per
// push. The decision paths (button press, typed command, REST → the
// dispatcher sees all of them) call MarkDecided; a single background
// worker drains the queue and edits the messages.

// decisionEdit is one pending message rewrite.
type decisionEdit struct {
	approvalID int64
	chatID     int64
	messageID  int64
	verdict    string // "approved" | "denied" | "expired"
	by         string
	at         time.Time
}

// EditRegistry remembers which pushed messages carry which approval.
type EditRegistry struct {
	mu   sync.Mutex
	msgs map[int64][]decisionTarget // approvalID → targets (multi-chat)
}

type decisionTarget struct {
	chatID    int64
	messageID int64
}

func NewEditRegistry() *EditRegistry {
	return &EditRegistry{msgs: make(map[int64][]decisionTarget)}
}

// Record stores where an approval's buttons were pushed.
func (r *EditRegistry) Record(approvalID, chatID, messageID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs[approvalID] = append(r.msgs[approvalID],
		decisionTarget{chatID: chatID, messageID: messageID})
}

// Targets returns (and keeps) the recorded targets.
func (r *EditRegistry) Targets(approvalID int64) []decisionTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]decisionTarget, len(r.msgs[approvalID]))
	copy(out, r.msgs[approvalID])
	return out
}

// editor drains decisionEdits and edits the recorded messages.
type editor struct {
	client Client
	reg    *EditRegistry
	logger *slog.Logger
	mu     sync.Mutex
	queue  []decisionEdit
	wake   chan struct{}
}

func newEditor(c Client, reg *EditRegistry, logger *slog.Logger) *editor {
	if logger == nil {
		logger = slog.Default()
	}
	return &editor{client: c, reg: reg, logger: logger, wake: make(chan struct{}, 1)}
}

// enqueue schedules an edit; safe from any decision path.
func (e *editor) enqueue(ed decisionEdit) {
	e.mu.Lock()
	e.queue = append(e.queue, ed)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// run drains until ctx is done.
func (e *editor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
			for {
				e.mu.Lock()
				if len(e.queue) == 0 {
					e.mu.Unlock()
					return
				}
				ed := e.queue[0]
				e.queue = e.queue[1:]
				e.mu.Unlock()
				e.apply(ctx, ed)
			}
		}
	}
}

// apply edits every recorded message for the approval; failure to edit
// is logged, never fatal (the ledger is the truth, the edit is polish).
func (e *editor) apply(ctx context.Context, ed decisionEdit) {
	targets := e.reg.Targets(ed.approvalID)
	icon := "🚫"
	if ed.verdict == "approved" {
		icon = "✅"
	}
	by := ed.by
	if by == "" {
		by = "unknown"
	}
	text := icon + " #" + strconv.FormatInt(ed.approvalID, 10) + " " + ed.verdict +
		" by " + by + " · " + ed.at.UTC().Format("15:04 UTC")
	for _, t := range targets {
		if err := e.client.EditMessageText(ctx, t.chatID, t.messageID, text); err != nil {
			// "message is not modified" and friends are benign races;
			// anything else still doesn't fail the decision.
			e.logger.Warn("telegram: decision edit failed",
				"approval_id", ed.approvalID, "chat_id", t.chatID, "error", err)
		}
	}
}

// NewEditor builds the drain worker.
func NewEditor(c Client, reg *EditRegistry, logger *slog.Logger) *Editor {
	return newEditor(c, reg, logger)
}

// Editor is the exported drain worker (construct via NewEditor).
type Editor = editor

// EnqueueDecided is the dispatcher hook: schedule the final-state edit.
func (e *editor) EnqueueDecided(approvalID int64, verdict, by string) {
	e.enqueue(decisionEdit{approvalID: approvalID, verdict: verdict,
		by: by, at: time.Now()})
}

// Run drains until ctx is done (call in a goroutine).
func (e *editor) Run(ctx context.Context) { e.run(ctx) }
