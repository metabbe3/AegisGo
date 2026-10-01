package app

import (
	"context"
	"strings"
	"testing"
)

// TestParseWatchRejectsPseudoFS pins the Hard-Rule-2 hardening: /watch
// must refuse kernel pseudo-filesystems outright — watching /dev/zero or
// /proc/cmdline is never a log watch and (pre-fix) fed size-based buffer
// allocation on a non-regular file.
func TestParseWatchRejectsPseudoFS(t *testing.T) {
	for _, bad := range []string{"/dev/zero.log", "/dev", "/proc/kmsg.log", "/sys/foo.log", "/proc"} {
		_, err := ParseWatch("/watch name=ab | path=" + bad + " | pattern=panic")
		if err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
}

// TestParseWatchRejectsNonLogExtension pins the extension allowlist:
// accidental watches on binaries, sockets, or dotfiles are rejected at
// parse time while real log paths (any directory) stay accepted.
func TestParseWatchRejectsNonLogExtension(t *testing.T) {
	for _, bad := range []string{"/tmp/app.bin", "/var/log/wtf", "/tmp/.hidden", "/tmp/a.log.exe"} {
		_, err := ParseWatch("/watch name=ab | path=" + bad + " | pattern=panic")
		if err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
	for _, ok := range []string{"/tmp/app.log", "/var/log/system.log", "/tmp/out.txt", "/tmp/e.ndjson"} {
		if _, err := ParseWatch("/watch name=ab | path=" + ok + " | pattern=panic"); err != nil {
			t.Fatalf("path %q rejected: %v", ok, err)
		}
	}
}

// TestAnalyzeCommandGuarded pins that /analyze applies the same parse-time
// path guard as /watch: pseudo-filesystems and non-log extensions are
// refused before logwatch.Analyze opens anything.
func TestAnalyzeCommandGuarded(t *testing.T) {
	g, _ := newWatchGate(t)
	for _, bad := range []string{"/dev/zero.log", "/proc/kmsg", "/tmp/app.bin"} {
		out := g.HandleWatchText(context.Background(), "/analyze "+bad)
		if out == "" || !strings.Contains(out, "⚠️") {
			t.Fatalf("/analyze %q accepted: %q", bad, out)
		}
	}
}
