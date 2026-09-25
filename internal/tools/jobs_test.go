package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitForJob polls until the job leaves "running" (or the deadline hits).
func waitForJob(t *testing.T, mgr *JobManager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := mgr.Get(id); ok && j.Status != JobRunning {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s still running after deadline", id)
	return Job{}
}

func TestJobManagerLifecycle(t *testing.T) {
	mgr := NewJobManager()
	// The job blocks until released so its "running" snapshot is observed,
	// not raced for: an instantly-returning fn can flip the job to done
	// before Get runs (this test used to flake under coverage that way).
	running := make(chan struct{})
	release := make(chan struct{})
	id := mgr.Start(context.Background(), time.Second, "test", map[string]string{"k": "v"},
		func(ctx context.Context) (any, error) {
			close(running)
			<-release
			return "all good", nil
		})
	if !strings.HasPrefix(id, "j_") {
		t.Errorf("job id = %q, want j_ prefix", id)
	}

	<-running // fn entered: the job stays running until release
	if j, ok := mgr.Get(id); !ok || j.Status != JobRunning || j.Kind != "test" || j.Meta["k"] != "v" {
		t.Errorf("running job = %+v (ok=%v)", j, ok)
	}
	close(release)
	done := waitForJob(t, mgr, id)
	if done.Status != JobDone || done.Result != "all good" || done.Error != "" {
		t.Errorf("finished job = %+v, want done with result", done)
	}
	if done.FinishedAt.IsZero() || done.FinishedAt.Before(done.CreatedAt) {
		t.Errorf("timestamps out of order: created=%v finished=%v", done.CreatedAt, done.FinishedAt)
	}
}

func TestJobManagerError(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), time.Second, "test", nil,
		func(ctx context.Context) (any, error) { return nil, errors.New("boom") })
	failed := waitForJob(t, mgr, id)
	if failed.Status != JobError || failed.Result != nil {
		t.Fatalf("job = %+v, want error with nil result", failed)
	}
	if !strings.Contains(failed.Error, "boom") {
		t.Errorf("error = %q, want it to carry the fn failure", failed.Error)
	}
}

// TestJobManagerSurvivesParentCancel pins the WithoutCancel detach: the HTTP
// request that started a job can go away (client disconnect, timeout) while
// the job itself runs to completion.
func TestJobManagerSurvivesParentCancel(t *testing.T) {
	mgr := NewJobManager()
	ctx, cancel := context.WithCancel(context.Background())
	id := mgr.Start(ctx, time.Second, "test", nil, func(ctx context.Context) (any, error) {
		<-ctx.Done() // wait for the job ctx to die (timeout), NOT the parent
		return "survived parent", nil
	})
	cancel() // the request context dies immediately
	j := waitForJob(t, mgr, id)
	if j.Status != JobDone || j.Result != "survived parent" {
		t.Errorf("job = %+v, want done despite parent cancel", j)
	}
}

func TestJobManagerTimeoutBounds(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), 30*time.Millisecond, "test", nil,
		func(ctx context.Context) (any, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return "took too long", nil
			}
		})
	j := waitForJob(t, mgr, id)
	if j.Status != JobError || !strings.Contains(j.Error, "deadline") {
		t.Errorf("job = %+v, want timeout error", j)
	}
}

func TestJobManagerUnknown(t *testing.T) {
	mgr := NewJobManager()
	if _, ok := mgr.Get("j_nope"); ok {
		t.Error("unknown job reported ok=true")
	}
}

// TestJobManagerRetention: finished jobs beyond the cap are evicted
// oldest-first; a running job is never evicted even when hundreds of jobs
// finish around it.
func TestJobManagerRetention(t *testing.T) {
	mgr := NewJobManager()
	// One long-running job started first: it must never be evicted while
	// running, no matter how many jobs finish around it.
	slowID := mgr.Start(context.Background(), 2*time.Second, "slow", nil,
		func(ctx context.Context) (any, error) {
			time.Sleep(400 * time.Millisecond)
			return "slow done", nil
		})
	// Then more finished jobs than the retention cap allows. They finish in
	// arbitrary order — awaiting them one-by-one would poll ids the cap has
	// already evicted, and the finished slice is itself capped, so the burst
	// is awaited by counting fn completions directly.
	const extra = maxFinishedJobs + 10
	var doneCount atomic.Int32
	ids := make([]string, extra)
	for i := range ids {
		ids[i] = mgr.Start(context.Background(), time.Second, "fast", nil,
			func(ctx context.Context) (any, error) { doneCount.Add(1); return i, nil })
	}
	deadline := time.Now().Add(5 * time.Second)
	for doneCount.Load() < extra && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := doneCount.Load(); got < extra {
		t.Fatalf("only %d/%d quick jobs finished in time", got, extra)
	}
	time.Sleep(20 * time.Millisecond) // let the last wrappers finish recording

	if j, ok := mgr.Get(slowID); !ok {
		t.Fatal("running job evicted by the retention cap")
	} else if j.Status != JobRunning && j.Status != JobDone {
		t.Fatalf("slow job = %+v, want running or done", j)
	}
	// The oldest finished jobs are gone; the newest survive.
	if _, ok := mgr.Get(ids[0]); ok {
		t.Error("oldest finished job survived retention cap")
	}
	if j, ok := mgr.Get(ids[extra-1]); !ok || j.Status != JobDone {
		t.Errorf("newest finished job = %+v ok=%v", j, ok)
	}
	waitForJob(t, mgr, slowID)
}

// TestJobManagerConcurrentStartGet hammers Start/Get from many goroutines —
// the manager must stay consistent under concurrent tool calls.
func TestJobManagerConcurrentStartGet(t *testing.T) {
	mgr := NewJobManager()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := mgr.Start(context.Background(), time.Second, "hammer", nil,
					func(ctx context.Context) (any, error) { return nil, nil })
				mgr.Get(id)
				mgr.Get("j_missing")
			}
		}()
	}
	wg.Wait()
}

func TestNewJobIDEntropy(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := newJobID()
		if seen[id] {
			t.Fatalf("collision on %q after %d ids", id, i)
		}
		seen[id] = true
	}
}

func TestJobStatusTool(t *testing.T) {
	mgr := NewJobManager()
	tl, err := NewJobStatus(mgr)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name() != "job_status" {
		t.Errorf("name = %q", tl.Name())
	}

	// Unknown job → loud error mentioning the per-process lifetime.
	if _, err := tl.Execute(context.Background(), []byte(`{"job_id":"j_ghost"}`)); err == nil {
		t.Fatal("unknown job accepted")
	} else if !strings.Contains(err.Error(), "unknown job") {
		t.Errorf("error = %v, want unknown-job wording", err)
	}

	// Known job → snapshot via both entry points (parity by construction:
	// same handler; assert the shapes line up).
	id := mgr.Start(context.Background(), time.Second, "test", map[string]string{"url": "u"},
		func(ctx context.Context) (any, error) { return DownloadResult{Bytes: 7}, nil })
	waitForJob(t, mgr, id)

	viaExec, err := tl.Execute(context.Background(), []byte(`{"job_id":`+quoteJSON(id)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	viaLLM, err := tl.FuncTool().Call(context.Background(), `{"job_id":`+quoteJSON(id)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	execJob, ok1 := viaExec.(JobStatusOutput)
	llmJob, ok2 := viaLLM.(JobStatusOutput)
	if !ok1 || !ok2 {
		t.Fatalf("types: %T vs %T", viaExec, viaLLM)
	}
	if execJob.JobID != id || llmJob.JobID != id || execJob.Status != JobDone || llmJob.Status != JobDone {
		t.Errorf("exec=%+v llm=%+v", execJob, llmJob)
	}
	if execJob.Meta["url"] != "u" || llmJob.Meta["url"] != "u" {
		t.Errorf("meta lost: exec=%+v llm=%+v", execJob, llmJob)
	}
	if res, ok := execJob.Result.(DownloadResult); !ok || res.Bytes != 7 {
		t.Errorf("result payload = %+v, want DownloadResult{Bytes:7}", execJob.Result)
	}
}

// TestJobManagerList covers List ordering: running first (oldest→newest),
// then finished newest-first, and the empty case.
func TestJobManagerList(t *testing.T) {
	// empty manager → nil/empty slice, no panic
	mgr := NewJobManager()
	if got := mgr.List(); len(got) != 0 {
		t.Errorf("empty List = %+v, want none", got)
	}
	// two finished jobs (timestamped order enforced by CreatedAt) + one running.
	// Jobs finish near-instantly so use distinct sleeps? No — deterministic:
	// finish first job, then start+hold the running one, then check ordering.
	id1 := mgr.Start(context.Background(), time.Second, "first", nil,
		func(ctx context.Context) (any, error) { return "1", nil })
	waitForJob(t, mgr, id1)
	id2 := mgr.Start(context.Background(), time.Second, "second", nil,
		func(ctx context.Context) (any, error) { return "2", nil })
	waitForJob(t, mgr, id2)
	entered := make(chan struct{})
	release := make(chan struct{})
	idR := mgr.Start(context.Background(), time.Second, "live", nil,
		func(ctx context.Context) (any, error) {
			close(entered)
			<-release
			return "r", nil
		})
	<-entered
	defer close(release)

	got := mgr.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	if got[0].Status != JobRunning || got[0].ID != idR {
		t.Errorf("first = %+v, want the running job %s", got[0], idR)
	}
	// finished newest-first: second before first
	if got[1].ID != id2 || got[2].ID != id1 {
		t.Errorf("finished order = %s,%s; want %s,%s", got[1].ID, got[2].ID, id2, id1)
	}
}

// TestListJobsAdapter pins the server.Deps adapter: same slice as List.
func TestListJobsAdapter(t *testing.T) {
	mgr := NewJobManager()
	id := mgr.Start(context.Background(), time.Second, "k", nil,
		func(ctx context.Context) (any, error) { return "v", nil })
	waitForJob(t, mgr, id)
	if got := mgr.ListJobs(); len(got) != 1 || got[0].ID != id {
		t.Errorf("ListJobs = %+v, want one job %s", got, id)
	}
}
