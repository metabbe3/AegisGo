package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DownloadOptions bounds and configures the download tool. Validation lives
// here — not in config.Validate() — because AEGIS_LLM=off mode skips
// Validate entirely; a negative cap must fail loudly in every mode.
type DownloadOptions struct {
	// TimeoutSecs bounds one download end-to-end (AEGIS_DOWNLOAD_TIMEOUT,
	// default 120).
	TimeoutSecs int
	// MaxBytes caps the downloaded size (AEGIS_DOWNLOAD_MAX_BYTES, default
	// 64 MiB). Oversize downloads fail and their partial file is removed.
	MaxBytes int64
	// AllowPrivate permits loopback/private-network URLs
	// (AEGIS_DOWNLOAD_ALLOW_PRIVATE=on). Off by default so a deployed agent
	// cannot be steered at internal endpoints; local e2e runs enable it.
	AllowPrivate bool
}

const (
	downloadDefaultTimeoutSecs = 120
	downloadDefaultMaxBytes    = int64(64 << 20) // 64 MiB
	downloadMaxRedirects       = 5
)

// withDefaults fills zero values and rejects nonsensical ones, naming the
// env var behind each knob (errors are user-facing).
func (o DownloadOptions) withDefaults() (DownloadOptions, error) {
	if o.TimeoutSecs == 0 {
		o.TimeoutSecs = downloadDefaultTimeoutSecs
	}
	if o.TimeoutSecs < 0 {
		return o, fmt.Errorf("AEGIS_DOWNLOAD_TIMEOUT must be positive (got %d)", o.TimeoutSecs)
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = downloadDefaultMaxBytes
	}
	if o.MaxBytes < 0 {
		return o, fmt.Errorf("AEGIS_DOWNLOAD_MAX_BYTES must be positive (got %d)", o.MaxBytes)
	}
	return o, nil
}

// DownloadInput is the schema for the download tool.
type DownloadInput struct {
	// URL to fetch; must be http:// or https://.
	URL string `json:"url"`
	// Path is the workspace-relative destination file. Missing parent
	// directories are created; an existing file at the path is replaced.
	Path string `json:"path"`
}

// DownloadJobOutput is the immediate result of download: the job has been
// started, not finished. Poll the id with the job_status tool.
type DownloadJobOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	URL    string `json:"url"`
	Path   string `json:"path"`
}

// DownloadResult is the job's Result payload once done.
type DownloadResult struct {
	Bytes int64  `json:"bytes"`
	URL   string `json:"url"`
	Path  string `json:"path"`
}

// NewDownload builds the download tool: workspace-bound destination,
// size-capped, SSRF-guarded, with each fetch running as a background job on
// the shared manager. net/http is used directly — no curl subprocess — so
// no user-influenced argv ever exists (cf. the system_command catalog).
func NewDownload(workspace string, mgr *JobManager, opts DownloadOptions) (Tool, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	client := downloadClient(opts)
	return New(Config{
		Name:        "download",
		Description: "Download a file over http(s) into the workspace as a background job. Returns a job_id immediately; poll it with job_status until done or error. Size-capped; loopback and private-network URLs are refused unless AEGIS_DOWNLOAD_ALLOW_PRIVATE=on.",
	}, func(ctx context.Context, in DownloadInput) (DownloadJobOutput, error) {
		return startDownload(ctx, workspace, mgr, opts, client, in)
	})
}

// downloadClient builds the fetch client for a configured download tool.
// Redirect hops are re-validated against the same host rules as the initial
// URL so a public address cannot bounce the fetch into an internal one.
func downloadClient(opts DownloadOptions) *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= downloadMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", downloadMaxRedirects)
			}
			if !opts.AllowPrivate && hostBlocked(req.URL.Hostname()) {
				return fmt.Errorf("redirect to blocked host %q", req.URL.Hostname())
			}
			return nil
		},
	}
}

func startDownload(ctx context.Context, workspace string, mgr *JobManager,
	opts DownloadOptions, client *http.Client, in DownloadInput) (DownloadJobOutput, error) {

	if err := checkDownloadURL(in.URL, opts.AllowPrivate); err != nil {
		return DownloadJobOutput{}, err
	}
	target, err := resolveNewPath(workspace, in.Path)
	if err != nil {
		return DownloadJobOutput{}, err
	}
	if st, statErr := os.Stat(target); statErr == nil && st.IsDir() {
		return DownloadJobOutput{}, fmt.Errorf("download target %q is a directory", in.Path)
	}

	dlURL, dlPath := in.URL, in.Path
	jobID := mgr.Start(ctx, time.Duration(opts.TimeoutSecs)*time.Second, "download",
		map[string]string{"url": dlURL, "path": dlPath},
		func(ctx context.Context) (any, error) {
			return runDownload(ctx, client, dlURL, dlPath, target, opts.MaxBytes)
		})
	return DownloadJobOutput{JobID: jobID, Status: JobRunning, URL: dlURL, Path: dlPath}, nil
}

// runDownload performs the fetch. It streams into a temp .part file next to
// the target and renames on success, so a failure or oversize abort never
// leaves a partial file at the final path.
func runDownload(ctx context.Context, client *http.Client, dlURL, dlPath, target string, maxBytes int64) (any, error) {
	// The parent of a validated in-workspace path is an in-workspace
	// ancestor; creating it can never escape.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("creating parent of %q: %w", dlPath, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".aegis-dl-*.part")
	if err != nil {
		return nil, fmt.Errorf("staging file for %q: %w", dlPath, err)
	}
	part := tmp.Name()
	fail := func(err error) (any, error) {
		tmp.Close()
		os.Remove(part)
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return fail(fmt.Errorf("building request for %q: %w", dlURL, err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(fmt.Errorf("fetching %q: %w", dlURL, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(fmt.Errorf("fetching %q: status %s", dlURL, resp.Status))
	}

	// Read maxBytes+1 so oversize is detected (and failed) rather than
	// silently truncated at the cap.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fail(fmt.Errorf("downloading %q: %w", dlURL, err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(part)
		return nil, fmt.Errorf("writing %q: %w", dlPath, err)
	}
	if n > maxBytes {
		os.Remove(part)
		return nil, fmt.Errorf("download %q exceeds the %d-byte cap (AEGIS_DOWNLOAD_MAX_BYTES)", dlURL, maxBytes)
	}
	if err := os.Rename(part, target); err != nil {
		os.Remove(part)
		return nil, fmt.Errorf("finalizing %q: %w", dlPath, err)
	}
	return DownloadResult{Bytes: n, URL: dlURL, Path: dlPath}, nil
}

// checkDownloadURL enforces the download tool's SSRF posture: http(s) only,
// and — unless AllowPrivate opted in — no loopback, private, link-local, or
// unspecified targets. Best-effort literal check: a public hostname that
// DNS-resolves into a private range is beyond this layer's scope (that is
// egress-proxy territory, not a template agent).
func checkDownloadURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	if allowPrivate {
		return nil
	}
	if hostBlocked(u.Hostname()) {
		return fmt.Errorf("url host %q is a loopback or private address (set AEGIS_DOWNLOAD_ALLOW_PRIVATE=on for local use)", u.Hostname())
	}
	return nil
}

// hostBlocked reports whether a hostname (already port-stripped by
// u.Hostname(), so IPv6 brackets cannot hide an address) is a literal
// loopback/private/link-local/unspecified target.
func hostBlocked(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}
