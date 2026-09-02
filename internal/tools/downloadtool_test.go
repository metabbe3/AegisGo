package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDownloadTool builds the download tool plus its manager against a temp
// workspace. All tests here run against in-process httptest loopback
// servers (no external network) and therefore enable AllowPrivate.
func newDownloadTool(t *testing.T, opts DownloadOptions) (Tool, *JobManager, string) {
	t.Helper()
	ws := t.TempDir()
	mgr := NewJobManager()
	if opts.TimeoutSecs == 0 {
		opts.TimeoutSecs = 5
	}
	tl, err := NewDownload(ws, mgr, opts)
	if err != nil {
		t.Fatal(err)
	}
	return tl, mgr, ws
}

// startDownloadTool runs the tool and returns the started job output.
func startDownloadTool(t *testing.T, tl Tool, url, path string) DownloadJobOutput {
	t.Helper()
	res, err := tl.Execute(context.Background(),
		[]byte(`{"url":`+quoteJSON(url)+`,"path":`+quoteJSON(path)+`}`))
	if err != nil {
		t.Fatalf("download %s: %v", url, err)
	}
	out, ok := res.(DownloadJobOutput)
	if !ok {
		t.Fatalf("download result type %T", res)
	}
	if out.Status != JobRunning || out.JobID == "" || out.URL != url || out.Path != path {
		t.Fatalf("job output = %+v, want running with id", out)
	}
	return out
}

func TestDownloadSuccess(t *testing.T) {
	body := strings.Repeat("aegis-download-fixture\n", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	out := startDownloadTool(t, tl, srv.URL+"/file.txt", "data/dl/file.txt")

	j := waitForJob(t, mgr, out.JobID)
	if j.Status != JobDone || j.Error != "" {
		t.Fatalf("job = %+v, want done", j)
	}
	res, ok := j.Result.(DownloadResult)
	if !ok || res.Bytes != int64(len(body)) || res.Path != "data/dl/file.txt" {
		t.Fatalf("result = %+v", j.Result)
	}
	got, err := os.ReadFile(filepath.Join(ws, "data", "dl", "file.txt"))
	if err != nil {
		t.Fatalf("downloaded file: %v", err)
	}
	if string(got) != body {
		t.Errorf("bytes differ: %d bytes on disk, want %d", len(got), len(body))
	}
	// The staged .part file must be gone — a finished download leaves only
	// the final artifact.
	parts, _ := filepath.Glob(filepath.Join(ws, "data", "dl", "*.part"))
	if len(parts) != 0 {
		t.Errorf("leftover part files: %v", parts)
	}
}

func TestDownloadHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	out := startDownloadTool(t, tl, srv.URL+"/nope.txt", "dl/nope.txt")

	j := waitForJob(t, mgr, out.JobID)
	if j.Status != JobError || !strings.Contains(j.Error, "404") {
		t.Fatalf("job = %+v, want error mentioning 404", j)
	}
	if _, err := os.Stat(filepath.Join(ws, "dl", "nope.txt")); !os.IsNotExist(err) {
		t.Error("failed download left a file at the target path")
	}
}

func TestDownloadOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true, MaxBytes: 128})
	out := startDownloadTool(t, tl, srv.URL+"/big.bin", "dl/big.bin")

	j := waitForJob(t, mgr, out.JobID)
	if j.Status != JobError || !strings.Contains(j.Error, "AEGIS_DOWNLOAD_MAX_BYTES") {
		t.Fatalf("job = %+v, want oversize error naming the env var", j)
	}
	// No partial file may survive an oversize failure.
	entries, _ := os.ReadDir(filepath.Join(ws, "dl"))
	if len(entries) != 0 {
		t.Errorf("oversize failure left files behind: %v", entries)
	}
}

func TestDownloadCreatesParentsAndReplaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "v2")
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	// Deep, not-yet-existing parents must be created.
	out := startDownloadTool(t, tl, srv.URL+"/f", "a/b/c/f.txt")
	j := waitForJob(t, mgr, out.JobID)
	if j.Status != JobDone {
		t.Fatalf("deep target: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a", "b", "c", "f.txt")); string(b) != "v2" {
		t.Error("deep-path download wrong bytes")
	}
	// Re-downloading over an existing file replaces it.
	out2 := startDownloadTool(t, tl, srv.URL+"/f", "a/b/c/f.txt")
	if j := waitForJob(t, mgr, out2.JobID); j.Status != JobDone {
		t.Fatalf("re-download over existing file: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a", "b", "c", "f.txt")); string(b) != "v2" {
		t.Error("replaced file wrong bytes")
	}
}

func TestDownloadTargetIsDirectory(t *testing.T) {
	tl, _, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	if err := os.MkdirAll(filepath.Join(ws, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := tl.Execute(context.Background(), []byte(`{"url":"http://example.com/f","path":"adir"}`))
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("err = %v, want directory-target wording", err)
	}
}

func TestDownloadEscapeRejected(t *testing.T) {
	tl, _, _ := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	for _, path := range []string{"../../etc/evil", "/tmp/evil"} {
		_, err := tl.Execute(context.Background(),
			[]byte(`{"url":"http://example.com/f","path":`+quoteJSON(path)+`}`))
		if err == nil {
			t.Errorf("path %q accepted", path)
		}
	}
}

// TestDownloadURLGuard pins the SSRF posture: without AllowPrivate, every
// literal loopback/private/link-local target is refused synchronously (no
// job starts, no fetch happens). With AllowPrivate, the same URL passes.
func TestDownloadURLGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tl, _, _ := newDownloadTool(t, DownloadOptions{AllowPrivate: false})
	blocked := []string{
		srv.URL + "/f",                       // 127.0.0.1 loopback literal
		"http://localhost:8080/f",            // localhost name
		"http://[::1]:8080/f",                // IPv6 loopback
		"http://10.1.2.3/f",                  // private range
		"http://192.168.0.9/f",               // private range
		"http://169.254.169.254/latest/meta", // link-local (cloud metadata)
		"http://0.0.0.0/f",                   // unspecified
		"file:///etc/passwd",                 // non-http scheme
		"ftp://example.com/f",                // non-http scheme
		"http:///nofhost",                    // empty host
	}
	for _, u := range blocked {
		if _, err := tl.Execute(context.Background(),
			[]byte(`{"url":`+quoteJSON(u)+`,"path":"out/f"}`)); err == nil {
			t.Errorf("url %q accepted without AllowPrivate", u)
		}
	}

	// The escape hatch flips the loopback case to allowed (job starts).
	allowed, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	out := startDownloadTool(t, allowed, srv.URL+"/f", "ok/f")
	if j := waitForJob(t, mgr, out.JobID); j.Status != JobDone {
		t.Errorf("allow-private job = %+v, want done", j)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "ok", "f")); string(b) != "ok" {
		t.Error("allow-private download wrong bytes")
	}
}

func TestHostBlocked(t *testing.T) {
	blocked := []string{
		"localhost", "LOCALHOST", "127.0.0.1", "::1", "10.0.0.1",
		"172.16.0.1", "192.168.1.4", "169.254.1.1", "0.0.0.0",
	}
	for _, h := range blocked {
		if !hostBlocked(h) {
			t.Errorf("hostBlocked(%q) = false, want true", h)
		}
	}
	allowed := []string{"example.com", "8.8.8.8", "2001:4860:4860::8888", "sub.example.com"}
	for _, h := range allowed {
		if hostBlocked(h) {
			t.Errorf("hostBlocked(%q) = true, want false", h)
		}
	}
}

// TestDownloadClientRedirectGuard drives the production client (built with
// AllowPrivate=false) directly: the initial loopback request is the test's
// own (the tool's URL gate is a separate layer), and the REDIRECT hop into
// another loopback host must be refused.
func TestDownloadClientRedirectGuard(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "internal")
	}))
	defer target.Close()

	hops := 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, target.URL+"/leaked", http.StatusFound)
	}))
	defer source.Close()

	client := downloadClient(DownloadOptions{})
	resp, err := client.Get(source.URL + "/start")
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect into loopback host accepted")
	}
	if !strings.Contains(err.Error(), "blocked host") {
		t.Errorf("err = %v, want blocked-host wording", err)
	}
	if hops != 1 {
		t.Errorf("hops = %d, want the redirect refused before a second hit", hops)
	}
}

func TestDownloadRedirectChainLimit(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/hop", http.StatusFound) // self-loop
	}))
	defer srv.Close()

	client := downloadClient(DownloadOptions{AllowPrivate: true})
	if _, err := client.Get(srv.URL + "/start"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want redirect-limit error", err)
	}
}

// TestDownloadSurvivesParentCancel: the tool's Execute returns before the
// fetch finishes; canceling the request context right after must not kill
// the in-flight download.
func TestDownloadSurvivesParentCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, "late but alive")
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true, TimeoutSecs: 5})
	ctx, cancel := context.WithCancel(context.Background())
	res, err := tl.Execute(ctx, []byte(`{"url":`+quoteJSON(srv.URL+"/slow")+`,"path":"slow/f.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.(DownloadJobOutput)
	cancel() // the request context dies; the job must not
	close(release)

	j := waitForJob(t, mgr, out.JobID)
	if j.Status != JobDone {
		t.Fatalf("job = %+v, want done despite request cancellation", j)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "slow", "f.txt")); string(b) != "late but alive" {
		t.Error("canceled-parent download wrong bytes")
	}
}

func TestNewDownloadValidation(t *testing.T) {
	ws := t.TempDir()
	mgr := NewJobManager()
	if _, err := NewDownload(ws, mgr, DownloadOptions{TimeoutSecs: -1}); err == nil ||
		!strings.Contains(err.Error(), "AEGIS_DOWNLOAD_TIMEOUT") {
		t.Errorf("negative timeout: err = %v, want it to name AEGIS_DOWNLOAD_TIMEOUT", err)
	}
	if _, err := NewDownload(ws, mgr, DownloadOptions{MaxBytes: -1}); err == nil ||
		!strings.Contains(err.Error(), "AEGIS_DOWNLOAD_MAX_BYTES") {
		t.Errorf("negative max bytes: err = %v, want it to name AEGIS_DOWNLOAD_MAX_BYTES", err)
	}
	// Zero values fall back to safe defaults, not an error.
	if _, err := NewDownload(ws, mgr, DownloadOptions{}); err != nil {
		t.Errorf("default options rejected: %v", err)
	}
}

// TestDownloadDualEntryParity: Execute and FuncTool().Call start real jobs
// at two different targets; both must land identical bytes.
func TestDownloadDualEntryParity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "parity-bytes")
	}))
	defer srv.Close()

	tl, mgr, ws := newDownloadTool(t, DownloadOptions{AllowPrivate: true})
	argsFor := func(path string) string {
		return `{"url":` + quoteJSON(srv.URL+"/p") + `,"path":` + quoteJSON(path) + `}`
	}

	viaExec, err := tl.Execute(context.Background(), []byte(argsFor("parity/exec.txt")))
	if err != nil {
		t.Fatal(err)
	}
	viaLLM, err := tl.FuncTool().Call(context.Background(), argsFor("parity/llm.txt"))
	if err != nil {
		t.Fatal(err)
	}
	execOut, ok1 := viaExec.(DownloadJobOutput)
	llmOut, ok2 := viaLLM.(DownloadJobOutput)
	if !ok1 || !ok2 {
		t.Fatalf("types: %T vs %T", viaExec, viaLLM)
	}
	waitForJob(t, mgr, execOut.JobID)
	waitForJob(t, mgr, llmOut.JobID)

	for _, p := range []string{"parity/exec.txt", "parity/llm.txt"} {
		b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(p)))
		if err != nil || string(b) != "parity-bytes" {
			t.Errorf("%s = %q (%v), want parity-bytes", p, b, err)
		}
	}
}
