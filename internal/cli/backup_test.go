// Tests for `aegis ctl backup` (#67): the on-demand twin of the daily
// 04:30 sidecar backup. Each test pins one promise: a run writes a real
// sidecar NEXT to the DB, `list` shows the history newest-first with a
// retention summary, and arg mistakes fail with usage instead of doing
// something creative.
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aegisgo/internal/store"
)

// TestCtlBackupRunWritesSidecar FAILS without the change: without the
// `backup` case in runCtl, the command errors with the usage line and
// no sidecar file appears next to the DB.
func TestCtlBackupRunWritesSidecar(t *testing.T) {
	dbPath := withDB(t, nil)
	var out bytes.Buffer
	if err := runCtl([]string{"backup"}, &out); err != nil {
		t.Fatalf("ctl backup: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "backup ok:") {
		t.Fatalf("output = %q, want 'backup ok:' line", got)
	}
	if !strings.Contains(got, ".sidecar") {
		t.Fatalf("output = %q, want the sidecar path", got)
	}
	// The artifact must physically exist next to the DB and open as a
	// real SQLite DB (VACUUM INTO snapshot, not a stub).
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "*.sidecar"))
	if len(matches) == 0 {
		t.Fatalf("no sidecar file next to the db at %s", filepath.Dir(dbPath))
	}
	ro, err := store.Open(matches[0])
	if err != nil {
		t.Fatalf("sidecar not openable as sqlite: %v", err)
	}
	ro.Close()
}

// TestCtlBackupListShowsHistory pins the listing contract: newest-first
// sidecar names, human sizes, and the retention summary line.
func TestCtlBackupListShowsHistory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ctl.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Audit(context.Background(), store.AuditEvent{TraceID: "b1", Interface: "test", Prompt: "seed", DecisionSource: "test"})
	if err := st.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	st.Close()
	t.Setenv("AEGIS_DB_PATH", dbPath)

	if err := runCtl([]string{"backup"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	// Forge two older sidecars so ordering is observable.
	for _, d := range []string{"20260101", "20260202"} {
		p := filepath.Join(dir, "ctl.db.backup-"+d+".sidecar")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runCtl([]string{"backup", "list"}, &out); err != nil {
		t.Fatalf("ctl backup list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "sidecar(s), retention 7") {
		t.Fatalf("output = %q, want retention summary", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 3 { // header lines + forged ones, then summary
		t.Fatalf("output = %q, want the sidecar rows", got)
	}
	// Date-named newest first: today's real sidecar must precede the
	// forged 2026-02-02 one.
	today := lines[0]
	feb := lines[1]
	if !strings.Contains(today, ".backup-") || strings.Contains(today, "20260202") {
		t.Fatalf("first row %q is not today's sidecar", today)
	}
	if !strings.Contains(feb, "20260202") {
		t.Fatalf("second row %q, want the forged 20260202 sidecar", feb)
	}
}

// TestCtlBackupListEmptyOnFreshBox: a dir with no sidecars is an honest
// empty listing, not an error.
func TestCtlBackupListEmptyOnFreshBox(t *testing.T) {
	withDB(t, nil)
	var out bytes.Buffer
	if err := runCtl([]string{"backup", "list"}, &out); err != nil {
		t.Fatalf("ctl backup list (fresh): %v", err)
	}
	if !strings.Contains(out.String(), "no sidecar backups yet") {
		t.Fatalf("output = %q, want the empty hint", out.String())
	}
}

// TestCtlBackupBadArgUsage: unknown args fail with usage BEFORE any
// backup runs (no sidecar side effects from a typo).
func TestCtlBackupBadArgUsage(t *testing.T) {
	dbPath := withDB(t, nil)
	var out bytes.Buffer
	err := runCtl([]string{"backup", "bogus"}, &out)
	if err == nil {
		t.Fatalf("expected usage error, got nil (out=%q)", out.String())
	}
	if !strings.Contains(err.Error(), "want list") {
		t.Fatalf("err = %v, want usage mentioning list", err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "*.sidecar"))
	if len(matches) != 0 {
		t.Fatalf("a bad arg must not write sidecars, found %v", matches)
	}
}

// days= arg validation is part of the stats surface the backup feature
// shares a usage line with; pin the reject branches (non-numeric, out of
// range) so the window knob cannot silently accept garbage.
func TestCtlStatsDaysValidation(t *testing.T) {
	withDB(t, nil)
	for _, bad := range []string{"days=abc", "days=0", "days=9999", "bogus=1"} {
		err := runCtl([]string{"stats", bad}, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("stats %q: expected error, got nil", bad)
		}
		if !strings.Contains(err.Error(), "days") && !strings.Contains(err.Error(), "want days=N") {
			t.Fatalf("stats %q: err = %v, want a days usage error", bad, err)
		}
	}
}
