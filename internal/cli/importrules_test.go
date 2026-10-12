// Tests for `aegis ctl rules import` (#66) — the offline admin twin of the
// /register_rules gate. Every test drives runCtl end-to-end with a temp DB
// and (for the -f= path) a temp file of /addrule lines, then reopens the
// store to verify rows actually landed (physical evidence, not self-report).
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

// importLine builds one /addrule replay line in the export dialect.
func importLine(name, pattern, tool string) string {
	return "/addrule name=" + name + " | pattern=" + pattern + " | tool=" + tool + ` | args={"path":"$1"}`
}

// importExportish mimics a real /rules export: header prose, replayable
// lines, and a reference-only footer — import must use ONLY the /addrule
// lines (var, not const: it is assembled from importLine).
var importExportish = "Rules export (2 live, 2 replayable as /addrule):\n" +
	importLine("notes_doc", `/notes(?:\s+(?<arg>.*))?`, "read_doc") + "\n" +
	importLine("tbl_read", `/tbl (?<f>\S+)`, "read_csv") + "\n" +
	"(2 live: 2 replayable as /addrule, 0 ship with the binary)"

func TestRulesImportStdinRoundTrip(t *testing.T) {
	withDB(t, nil)
	old := os.Stdin
	tmp := t.TempDir()
	f := filepath.Join(tmp, "export.txt")
	if err := os.WriteFile(f, []byte(importExportish), 0o600); err != nil {
		t.Fatalf("writing stdin fixture: %v", err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatalf("opening stdin fixture: %v", err)
	}
	os.Stdin = rf
	t.Cleanup(func() { os.Stdin = old; rf.Close() })

	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "import"}, &buf); err != nil {
		t.Fatalf("rules import: %v", err)
	}
	if !strings.Contains(buf.String(), "imported 2 rule(s)") {
		t.Errorf("output %q, want \"imported 2 rule(s)\"", buf.String())
	}
	// Both rows must physically exist in the reopened store.
	st, err := store.Open(os.Getenv("AEGIS_DB_PATH"))
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	rules, err := st.RuleStates(ctx)
	if err != nil {
		t.Fatalf("reading rule states: %v", err)
	}
	have := map[string]bool{}
	for _, r := range rules {
		have[r["name"].(string)] = true
	}
	for _, want := range []string{"notes_doc", "tbl_read"} {
		if !have[want] {
			t.Errorf("rule %q missing after import (have %v)", want, have)
		}
	}
}

func TestRulesImportFileFlagAndIdempotentReplay(t *testing.T) {
	withDB(t, nil)
	tmp := t.TempDir()
	f := filepath.Join(tmp, "export.txt")
	if err := os.WriteFile(f, []byte(importExportish), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "import", "-f=" + f}, &buf); err != nil {
		t.Fatalf("rules import -f: %v", err)
	}
	// Re-importing the same export converges (idempotent upsert): still
	// 2 imported — NOT 4 rows.
	buf.Reset()
	if err := runCtl([]string{"rules", "import", "-f=" + f}, &buf); err != nil {
		t.Fatalf("rules import -f replay: %v", err)
	}
	if !strings.Contains(buf.String(), "imported 2 rule(s)") {
		t.Errorf("replay output %q, want \"imported 2 rule(s)\" again (idempotent)", buf.String())
	}
	st, err := store.Open(os.Getenv("AEGIS_DB_PATH"))
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st.Close()
	rules, err := st.RuleStates(context.Background())
	if err != nil {
		t.Fatalf("reading rule states: %v", err)
	}
	count := 0
	for _, r := range rules {
		if r["name"] == "notes_doc" || r["name"] == "tbl_read" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("imported rule rows = %d after replay, want 2 (no duplicates)", count)
	}
}

func TestRulesImportBadLineRejectsWholeBatch(t *testing.T) {
	withDB(t, nil)
	batch := importLine("good_one", `/good(?:\s+(?<arg>.*))?`, "read_doc") + "\n" +
		"/addrule name=bad rule | pattern=/x | tool=read_doc\n" // bad: junk segment
	old := os.Stdin
	tmp := t.TempDir()
	f := filepath.Join(tmp, "bad.txt")
	if err := os.WriteFile(f, []byte(batch), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	os.Stdin = rf
	t.Cleanup(func() { os.Stdin = old; rf.Close() })

	var buf bytes.Buffer
	err = runCtl([]string{"rules", "import"}, &buf)
	errMustContain(t, err, "line ") // parse failure names the offending line
	// Batch atomicity: the GOOD line must not have landed either.
	st, serr := store.Open(os.Getenv("AEGIS_DB_PATH"))
	if serr != nil {
		t.Fatalf("reopening store: %v", serr)
	}
	defer st.Close()
	rules, rerr := st.RuleStates(context.Background())
	if rerr != nil {
		t.Fatalf("reading rule states: %v", rerr)
	}
	for _, r := range rules {
		if r["name"] == "good_one" {
			t.Errorf("rule good_one landed despite batch rejection — batch must be atomic")
		}
	}
}

func TestRulesImportDeniesNonChatAddableTool(t *testing.T) {
	withDB(t, nil)
	// system_command is exportable as reference-only; replaying it as an
	// import must be rejected (Hard Rule 12 symmetry).
	batch := "/addrule name=oops | pattern=/oops | tool=system_command | args={\"cmd\":\"uptime\"}\n"
	old := os.Stdin
	tmp := t.TempDir()
	f := filepath.Join(tmp, "ref.txt")
	if err := os.WriteFile(f, []byte(batch), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	os.Stdin = rf
	t.Cleanup(func() { os.Stdin = old; rf.Close() })

	var buf bytes.Buffer
	err = runCtl([]string{"rules", "import"}, &buf)
	errMustContain(t, err, "not addable from chat")
}

func TestRulesImportNoAddruleLines(t *testing.T) {
	withDB(t, nil)
	old := os.Stdin
	tmp := t.TempDir()
	f := filepath.Join(tmp, "empty.txt")
	if err := os.WriteFile(f, []byte("Rules export (0 live):\n(0 live: 0 replayable, 0 ship with the binary)"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	os.Stdin = rf
	t.Cleanup(func() { os.Stdin = old; rf.Close() })

	var buf bytes.Buffer
	err = runCtl([]string{"rules", "import"}, &buf)
	errMustContain(t, err, "no /addrule lines found")
}

func TestRulesImportBadFlag(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl([]string{"rules", "import", "--file"}, &buf)
	errMustContain(t, err, "unknown import arg")
}

func TestRulesImportMissingFile(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl([]string{"rules", "import", "-f=/nonexistent/export.txt"}, &buf)
	errMustContain(t, err, "open /nonexistent/export.txt")
}
