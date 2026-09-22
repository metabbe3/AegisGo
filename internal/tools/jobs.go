package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"aegisgo/internal/task"
)

// Job lifecycle statuses. They deliberately mirror the store's answers
// vocabulary (pending/done/error) without importing internal/store: the
// tools package stays store-free, and a "running" job genuinely differs from
// a "pending" answer — a job holds a goroutine and filesystem resources.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobError   = "error"
)

// maxFinishedJobs bounds how many finished jobs stay queryable. Bounded
// retention keeps the registry cheap (Hard Rule 10 applies to memory too);
// running jobs are never evicted.
const maxFinishedJobs = 256

// Job is the snapshot of one background job. Always handled by value —
// Get returns a copy, and only the manager's wrapper goroutine mutates the
// original, under the manager lock.
type Job struct {
	ID         string            `json:"job_id"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
	Error      string            `json:"error,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
	Result     any               `json:"result,omitempty"`
}

// JobManager tracks background jobs started by tools (downloads today). One
// manager is created per process and shared, so the job_status tool sees
// every job any tool started. Deliberately in-memory: a job lives exactly
// as long as the process running it — poll from the same `aegis serve` process, not
// from a fresh CLI invocation.
type JobManager struct {
	mu       sync.RWMutex
	jobs     map[string]*Job
	finished []string // finish order, oldest first — drives retention eviction
	tasks    task.Group
}

// NewJobManager returns an empty manager.
func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[string]*Job)}
}

// Start registers a running job and launches fn on its own goroutine,
// returning the job id immediately. fn runs under a context detached from
// the caller's but bounded by timeout (task.Group.Go): a background job
// must outlive the HTTP request that started it, yet never run forever.
// The wrapper here is the only writer of the job's outcome — callers pass a
// plain function, never a *Job — so jobs are race-free by construction.
func (m *JobManager) Start(parent context.Context, timeout time.Duration,
	kind string, meta map[string]string, fn func(ctx context.Context) (any, error)) string {

	id := newJobID()
	m.mu.Lock()
	m.jobs[id] = &Job{ID: id, Kind: kind, Status: JobRunning, CreatedAt: time.Now(), Meta: meta}
	m.mu.Unlock()

	m.tasks.Go(parent, timeout, func(ctx context.Context) {
		res, err := fn(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		// Retention never drops a running job, so the lookup always hits;
		// guard anyway so a future change cannot turn into a nil write.
		if j, ok := m.jobs[id]; ok {
			j.FinishedAt = time.Now()
			if err != nil {
				j.Status = JobError
				j.Error = err.Error()
			} else {
				j.Status = JobDone
				j.Result = res
			}
		}
		m.finished = append(m.finished, id)
		m.evictFinishedLocked()
	})
	return id
}

// evictFinishedLocked drops the oldest finished jobs beyond the retention
// cap. Caller must hold m.mu.
func (m *JobManager) evictFinishedLocked() {
	for len(m.finished) > maxFinishedJobs {
		delete(m.jobs, m.finished[0])
		m.finished = m.finished[1:]
	}
}

// Get returns a copy of the job, if still retained.
func (m *JobManager) Get(id string) (Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// newJobID mints an unguessable id ("j_" + 8 random bytes hex).
func newJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is a broken system; degrade to a time-derived
		// id rather than panic in a tool path.
		return fmt.Sprintf("j_%x", time.Now().UnixNano())
	}
	return "j_" + hex.EncodeToString(b)
}

// JobStatusInput is the schema for the job_status tool.
type JobStatusInput struct {
	// JobID returned by the tool that started a background job (e.g. download).
	JobID string `json:"job_id"`
}

// JobStatusOutput is the result of the job_status tool: a Job snapshot.
// A dedicated struct (rather than Job itself) because the LLM path derives
// a schema from the output type — and two framework quirks shape it: any
// omitempty (non-required) property on a struct output breaks functool's
// default application, and a nil map fails schema validation. So: no
// omitempty anywhere, and Meta is always non-nil.
type JobStatusOutput struct {
	JobID      string            `json:"job_id"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Error      string            `json:"error"`
	Meta       map[string]string `json:"meta"`
	Result     any               `json:"result"`
}

// output converts a Job snapshot into the schematizable tool output.
func (j Job) output() JobStatusOutput {
	meta := j.Meta
	if meta == nil {
		meta = map[string]string{}
	}
	return JobStatusOutput{
		JobID: j.ID, Kind: j.Kind, Status: j.Status,
		CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt,
		Error: j.Error, Meta: meta, Result: j.Result,
	}
}

// NewJobStatus builds the job_status tool over a shared JobManager.
func NewJobStatus(mgr *JobManager) (Tool, error) {
	return New(Config{
		Name:        "job_status",
		Description: "Check a background job (e.g. a download): running, done, or error, with its result or failure message. Poll until the status leaves \"running\".",
	}, func(ctx context.Context, in JobStatusInput) (JobStatusOutput, error) {
		j, ok := mgr.Get(in.JobID)
		if !ok {
			return JobStatusOutput{}, fmt.Errorf("unknown job %q (jobs live only inside the process that started them)", in.JobID)
		}
		return j.output(), nil
	})
}
