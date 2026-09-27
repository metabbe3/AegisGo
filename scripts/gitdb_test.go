package scripts

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoSQLiteDBTracked pins the repo invariant that no SQLite database
// files are ever committed: a stray one-shot `aegis agent` run creates
// aegisgo.db in the process cwd, and a tracked live DB both bloats the
// repo and invites WAL/shm sidecars into commits. The live DB lives
// outside the repo (AEGIS_DB_PATH points into aegisgo-growth).
func TestNoSQLiteDBTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		for _, suffix := range []string{".db", ".db-shm", ".db-wal"} {
			if strings.HasSuffix(line, suffix) {
				t.Fatalf("SQLite file %q is tracked by git — remove it (git rm --cached) and rely on the *.db gitignore rule", line)
			}
		}
	}
}
