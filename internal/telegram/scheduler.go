package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"aegisgo/internal/store"
)

// schedPersister is the slice of *store.Store the scheduler needs for
// durable schedules. Nil (unwired) keeps the scheduler memory-only — the
// pre-v8 behavior, used by narrow tests.
type schedPersister interface {
	SaveSchedule(ctx context.Context, chatID int64, command string, intervalMs int64) (int64, error)
	DeleteSchedule(ctx context.Context, id int64) (bool, error)
	ListSchedules(ctx context.Context) ([]store.SchedJob, error)
}

// Scheduler runs user-registered text commands on a fixed cadence and sends
// the engine's reply to the allowlisted chat that registered them. When a
// persister is wired (SetSchedules → store), schedules are durable: rows
// survive restarts and rehydrate at Start — nightly deploys no longer wipe
// them (store migration v8).
type Scheduler struct {
	eng     engineRunner
	client  Client
	logger  *slog.Logger
	allowed map[int64]bool
	persist schedPersister

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

// SetSchedules wires durable persistence; without it the scheduler stays
// memory-only (schedules silently lost on restart — legacy behavior).
func (s *Scheduler) SetSchedules(p schedPersister) { s.persist = p }

// StartSchedules wires the persister and launches all jobs at boot —
// the one-line wiring the app layer calls (rehydrate + start).
func (d *Dispatcher) StartSchedules(ctx context.Context, p schedPersister) {
	d.sched.SetSchedules(p)
	d.sched.Start(ctx)
}

// Start rehydrates persisted schedules (when a store is wired) and launches
// every job. Call once at boot. Restart semantics: next run = now +
// interval — a restart never fires a catch-up burst of stale runs. Rows
// whose chat fell off the allowlist (or that sit below the minute floor)
// stay in the DB but are not launched — the allowlist remains the boundary.
func (s *Scheduler) Start(ctx context.Context) {
	if s.persist != nil {
		rows, err := s.persist.ListSchedules(ctx)
		if err != nil {
			s.logger.Error("scheduler: rehydrate failed", "error", err)
		} else {
			s.mu.Lock()
			for _, r := range rows {
				if !s.allowed[r.ChatID] {
					continue
				}
				interval := time.Duration(r.IntervalMs) * time.Millisecond
				if interval < time.Minute {
					continue // legacy/manual row below the parse-time floor
				}
				j := &cronJob{id: r.ID, chatID: r.ChatID, command: r.Command,
					interval: interval, nextRun: time.Now().Add(interval),
					stop: make(chan struct{})}
				s.jobs[j.id] = j
				if j.id > s.next {
					s.next = j.id
				}
			}
			s.mu.Unlock()
			if len(rows) > 0 {
				s.logger.Info("scheduler: rehydrated", "rows", len(rows))
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		s.launchLocked(ctx, j)
	}
}

// Register parses "/every 30m <command>" (also 1h, 45s rejected: minute
// floor). Replies with the job id used by /unschedule. With a persister
// wired the row lands in SQLite first and the DB id becomes the job id —
// a persist failure answers honestly and registers nothing.
func (s *Scheduler) Register(ctx context.Context, chatID int64, text string) string {
	interval, command, err := parseEvery(text)
	if err != nil {
		return err.Error()
	}
	if !s.allowed[chatID] {
		return "scheduler: chat not allowlisted"
	}
	var id int64
	if s.persist != nil {
		id, err = s.persist.SaveSchedule(ctx, chatID, command, interval.Milliseconds())
		if err != nil {
			s.logger.Error("scheduler: persist failed", "error", err)
			return "scheduler: could not save the schedule (storage error) — nothing registered"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist == nil {
		s.next++
		id = s.next
	}
	j := &cronJob{id: id, chatID: chatID, command: command,
		interval: interval, nextRun: time.Now().Add(interval), stop: make(chan struct{})}
	s.jobs[j.id] = j
	s.launchLocked(context.WithoutCancel(context.Background()), j)
	return fmt.Sprintf("scheduled #%d: %s every %s", j.id, command, interval)
}

// Unregister removes a job by "#<id>"; replies human-readable result. With
// a persister wired the row is deleted too — a delete failure stops the
// live job but warns that it may return after restart (honest, not silent).
func (s *Scheduler) Unregister(ctx context.Context, text string) string {
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
	if s.persist != nil {
		if _, err := s.persist.DeleteSchedule(ctx, id); err != nil {
			s.logger.Error("scheduler: delete persist failed", "job", id, "error", err)
			return fmt.Sprintf("stopped #%d, but removing it failed — it may return after restart", id)
		}
	}
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
