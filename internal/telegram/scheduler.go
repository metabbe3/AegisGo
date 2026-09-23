package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"
)

// Scheduler runs user-registered text commands on a fixed cadence and sends
// the engine's reply to the allowlisted chat that registered them. Purely
// in-memory: restart forgets schedules (documented — persistence lands with
// a future store-backed iteration if the feature earns it).
type Scheduler struct {
	eng     engineRunner
	client  Client
	logger  *slog.Logger
	allowed map[int64]bool

	mu   sync.Mutex
	next int64
	jobs map[int64]*cronJob
}

type cronJob struct {
	id       int64
	chatID   int64
	command  string        // full text, e.g. "/disk"
	interval time.Duration // fixed cadence, minimum 1 minute
	nextRun  time.Time
	stop     chan struct{}
}

// NewScheduler builds a scheduler. interval on Run() must be >= 1 minute —
// Telegram is not a high-frequency pipe (flood limits start at ~30 msg/s
// but the useful floor for ops commands is a minute).
func NewScheduler(e engineRunner, c Client, allowChats []int64, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	allowed := make(map[int64]bool, len(allowChats))
	for _, id := range allowChats {
		allowed[id] = true
	}
	return &Scheduler{eng: e, client: c, logger: logger,
		allowed: allowed, jobs: map[int64]*cronJob{}}
}

// Start launches all registered jobs; call once at boot.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		s.launchLocked(ctx, j)
	}
}

// Register parses "/every 30m <command>" (also 1h, 45s rejected: minute
// floor). Replies with the job id used by /unschedule.
func (s *Scheduler) Register(chatID int64, text string) string {
	interval, command, err := parseEvery(text)
	if err != nil {
		return err.Error()
	}
	if !s.allowed[chatID] {
		return "scheduler: chat not allowlisted"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	j := &cronJob{id: s.next, chatID: chatID, command: command,
		interval: interval, nextRun: time.Now().Add(interval), stop: make(chan struct{})}
	s.jobs[j.id] = j
	s.launchLocked(context.WithoutCancel(context.Background()), j)
	return fmt.Sprintf("scheduled #%d: %s every %s", j.id, command, interval)
}

// Unregister removes a job by "#<id>"; replies human-readable result.
func (s *Scheduler) Unregister(text string) string {
	id, err := parseUnschedule(text)
	if err != nil {
		return err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Sprintf("no scheduled job #%d", id)
	}
	delete(s.jobs, id)
	close(j.stop)
	return fmt.Sprintf("unscheduled #%d (%s)", id, j.command)
}

// List renders active jobs for the chat.
func (s *Scheduler) List() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return "no scheduled jobs"
	}
	var b strings.Builder
	for _, j := range s.jobs {
		next := time.Until(j.nextRun).Round(time.Second)
		fmt.Fprintf(&b, "#%d every %s: %s (next %s)\n", j.id, j.interval, j.command, next)
	}
	return strings.TrimRight(b.String(), "\n")
}

// launchLocked starts the goroutine for a job; caller holds s.mu.
func (s *Scheduler) launchLocked(ctx context.Context, j *cronJob) {
	go func() {
		t := time.NewTimer(time.Until(j.nextRun))
		defer t.Stop()
		for {
			select {
			case <-j.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			res := s.eng.Run(rctx, j.command)
			cancel()
			if res.Answer != "" {
				if _, err := s.client.SendMessage(rctx, j.chatID,
					fmt.Sprintf("⏰ #%d %s\n%s", j.id, j.command, res.Answer), 0); err != nil {
					s.logger.Error("scheduler: send failed", "job", j.id, "error", err)
				}
			}
			j.nextRun = time.Now().Add(j.interval)
			t.Reset(j.interval)
		}
	}()
}

// parseEvery accepts "every <dur> <command...>" with a leading optional
// "/". Minimum 1 minute; hours/days allowed.
func parseEvery(text string) (time.Duration, string, error) {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "/")
	fields := strings.Fields(t)
	if len(fields) < 3 || fields[0] != "every" {
		return 0, "", fmt.Errorf("usage: /every 30m /disk")
	}
	d, err := time.ParseDuration(fields[1])
	if err != nil || d < time.Minute {
		return 0, "", fmt.Errorf("interval must be ≥ 1m (e.g. 30m, 1h, 6h)")
	}
	return d, strings.Join(fields[2:], " "), nil
}

// parseUnschedule accepts "/unschedule #3" or "/unschedule 3".
func parseUnschedule(text string) (int64, error) {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
	fields := strings.Fields(t)
	if len(fields) != 2 || fields[0] != "unschedule" {
		return 0, fmt.Errorf("usage: /unschedule #3")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("usage: /unschedule #3")
	}
	return id, nil
}
