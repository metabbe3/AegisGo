// Tests for the aegis ctl subcommand. runCtl writes to injected stdout and
// reads the store at AEGIS_DB_PATH, so each test seeds a temp database
// through the store API (fire-and-forget writes + Flush, exactly like the
// engine's request path) and then points the command at it.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"aegisgo/internal/store"
)

// withDB opens a fresh store at a temp path, lets seed populate it, flushes
// the single-writer queue, closes, and points AEGIS_DB_PATH at the file.
// Closing drains too, but flushing keeps the barrier explicit.
func withDB(t *testing.T, seed func(ctx context.Context, st *store.Store)) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ctl.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening seed store: %v", err)
	}
	ctx := context.Background()
	if seed != nil {
		seed(ctx, st)
	}
	if err := st.Flush(ctx); err != nil {
		t.Fatalf("flushing seed writes: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing seed store: %v", err)
	}
	t.Setenv("AEGIS_DB_PATH", dbPath)
	// Blank the threshold knob so `rules mine` defaults match the binary's,
	// whatever the developer's shell carries.
	t.Setenv("AEGIS_MINER_THRESHOLD", "")
	return dbPath
}

// seedRule inserts one rule row in the exact shape router seeding and the
// miner write them.
func seedRule(ctx context.Context, st *store.Store, name, pattern, tool, origin string) {
	if err := st.Exec(ctx,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts)
		 VALUES (?,?,?,?,?,1,?)`,
		name, pattern, tool, `{"path":"$1"}`, origin,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		panic(err)
	}
}

// ruleRow reopens the database read-only-in-spirit and returns one rule's
// state/enabled pair, the fields promote/demote flip.
func ruleRow(t *testing.T, dbPath, name string) (string, bool) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st.Close()
	rules, err := st.RuleStates(context.Background())
	if err != nil {
		t.Fatalf("reading rule states: %v", err)
	}
	for _, r := range rules {
		if r["name"] == name {
			state, _ := r["state"].(string)
			enabled, _ := r["enabled"].(bool)
			return state, enabled
		}
	}
	t.Fatalf("rule %q not found", name)
	return "", false
}

// errMustContain fails unless err is non-nil mentioning want.
func errMustContain(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestCtlUsageNoArgs(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl(nil, &buf)
	errMustContain(t, err, "usage: aegis ctl rules list")
}

func TestCtlUsageUnknownCommand(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl([]string{"frobnicate"}, &buf)
	errMustContain(t, err, "usage: aegis ctl rules")
}

// TestRulesListSeeded: the table renders a header plus one aligned row per
// rule, tab-separated columns expanded by the tabwriter.
func TestRulesListSeeded(t *testing.T) {
	withDB(t, func(ctx context.Context, st *store.Store) {
		seedRule(ctx, st, "uptime", "/uptime", "system_command", "seed")
		seedRule(ctx, st, "mined_1", `summarize\s+(\S+)`, "read_csv", "mined")
	})
	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "list"}, &buf); err != nil {
		t.Fatalf("rules list: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "PATTERN") {
		t.Errorf("rules list output %q, want the header row", out)
	}
	// One data row, columns in order: name state origin enabled tool pattern.
	row := regexp.MustCompile(`(?m)^uptime\s+active\s+seed\s+true\s+system_command\s+/uptime$`)
	if !row.MatchString(out) {
		t.Errorf("rules list output %q, want a matched uptime row", out)
	}
	minedRow := regexp.MustCompile(`(?m)^mined_1\s+active\s+mined\s+true\s+read_csv\s+`)
	if !minedRow.MatchString(out) {
		t.Errorf("rules list output %q, want a matched mined_1 row", out)
	}
}

func TestRulesMineNone(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "mine", "--threshold", "2"}, &buf); err != nil {
		t.Fatalf("rules mine: %v", err)
	}
	if want := "no eligible clusters"; !strings.Contains(buf.String(), want) {
		t.Errorf("rules mine output %q, want %q", buf.String(), want)
	}
}

// TestRulesMineProposes: a cluster of identical fallback shapes whose runs
// agree on one derivable tool becomes a shadow rule named mined_1, printed
// with its synthesized pattern and tool.
func TestRulesMineProposes(t *testing.T) {
	withDB(t, func(ctx context.Context, st *store.Store) {
		for i := 0; i < 2; i++ {
			st.RecordFallback(store.FallbackEvent{
				TraceID:          fmt.Sprintf("seed-%d", i),
				NormalizedPrompt: "summarize <path>",
				RawPrompt:        "summarize ./logs.csv",
				ToolsUsed:        "read_csv",
			})
		}
	})
	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "mine", "--threshold", "2"}, &buf); err != nil {
		t.Fatalf("rules mine: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "shadow rule mined_1") {
		t.Errorf("rules mine output %q, want the mined_1 proposal", out)
	}
	if !strings.Contains(out, "→ read_csv (cluster 2)") {
		t.Errorf("rules mine output %q, want the tool and cluster size", out)
	}
	if !strings.Contains(out, "next: drive traffic") {
		t.Errorf("rules mine output %q, want the follow-up hint", out)
	}
}

func TestRulesMineBadFlag(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl([]string{"rules", "mine", "--bogus"}, &buf)
	errMustContain(t, err, "bogus")
}

// TestRulesPromoteDemoteRoundTrip drives both lifecycle transitions end to
// end (command → store row) and the usage errors around them.
func TestRulesPromoteDemoteRoundTrip(t *testing.T) {
	dbPath := withDB(t, func(ctx context.Context, st *store.Store) {
		seedRule(ctx, st, "mined_1", `summarize\s+(\S+)`, "read_csv", "mined")
	})

	var buf bytes.Buffer
	if err := runCtl([]string{"rules", "demote", "mined_1"}, &buf); err != nil {
		t.Fatalf("rules demote: %v", err)
	}
	if state, enabled := ruleRow(t, dbPath, "mined_1"); state != "demoted" || enabled {
		t.Errorf("after demote: state=%q enabled=%v, want demoted/false", state, enabled)
	}
	if err := runCtl([]string{"rules", "promote", "mined_1"}, &buf); err != nil {
		t.Fatalf("rules promote: %v", err)
	}
	if state, enabled := ruleRow(t, dbPath, "mined_1"); state != "active" || !enabled {
		t.Errorf("after promote: state=%q enabled=%v, want active/true", state, enabled)
	}

	// Missing-name and unknown-subcommand usage errors.
	for _, args := range [][]string{
		{"rules"},
		{"rules", "promote"},
		{"rules", "demote"},
	} {
		err := runCtl(args, &buf)
		if err == nil || !strings.Contains(err.Error(), "usage: aegis ctl rules") {
			t.Errorf("runCtl(%v) error = %v, want a rules usage error", args, err)
		}
	}
	errMustContain(t, runCtl([]string{"rules", "nope"}, &buf), `unknown rules subcommand "nope"`)
}

func TestStatsJSON(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	if err := runCtl([]string{"stats"}, &buf); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !json.Valid(buf.Bytes()) {
		t.Errorf("stats output is not valid JSON:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"total_runs"`) {
		t.Errorf("stats output %q, want a total_runs field", buf.String())
	}
}

// TestReplayKnownTrace: a seeded audit row replays as the header plus one
// matched row carrying its interface and decision source.
func TestReplayKnownTrace(t *testing.T) {
	withDB(t, func(ctx context.Context, st *store.Store) {
		st.Audit(ctx, store.AuditEvent{
			TraceID: "trace-known", Interface: store.IFaceCLI,
			DecisionSource: store.SourceRouter, RuleID: "uptime",
			Prompt: "/uptime", LatencyMS: 3,
		})
	})
	var buf bytes.Buffer
	if err := runCtl([]string{"replay", "trace-known"}, &buf); err != nil {
		t.Fatalf("replay: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "IFACE") || !strings.Contains(out, "SOURCE") {
		t.Errorf("replay output %q, want the header row", out)
	}
	row := regexp.MustCompile(`(?m)^.*\scli\s+regex_router\s+uptime\s+`)
	if !row.MatchString(out) {
		t.Errorf("replay output %q, want a cli/regex_router/uptime row", out)
	}
}

func TestReplayUnknown(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	err := runCtl([]string{"replay", "no-such-trace"}, &buf)
	errMustContain(t, err, "no audit rows for trace no-such-trace")
}

func TestReplayArgCount(t *testing.T) {
	withDB(t, nil)
	var buf bytes.Buffer
	for _, args := range [][]string{{"replay"}, {"replay", "a", "b"}} {
		err := runCtl(args, &buf)
		errMustContain(t, err, "usage: aegis ctl replay <trace-id>")
	}
}

// TestCtlJobs covers the ctl jobs client against a fake /v1/jobs server:
// table rendering, empty state, bearer header when configured, and the
// connection-refused error path.
func TestCtlJobs(t *testing.T) {
	// fake server serving one job + capturing the auth header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t0k" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"job_id":"j_ab12","kind":"download","status":"running","created_at":"2026-09-25T09:00:00Z","finished_at":"0001-01-01T00:00:00Z","error":"","meta":{},"result":null}]`)
	}))
	defer srv.Close()
	t.Setenv("AEGIS_ADDR", srv.URL)
	t.Setenv("AEGIS_HTTP_TOKEN", "t0k")
	var out bytes.Buffer
	if err := runCtl([]string{"jobs"}, &out); err != nil {
		t.Fatalf("ctl jobs: %v", err)
	}
	if !strings.Contains(out.String(), "j_ab12") || !strings.Contains(out.String(), "running") {
		t.Errorf("output = %q, want job row", out.String())
	}

	// empty list → friendly line, still exit 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv2.Close()
	t.Setenv("AEGIS_ADDR", srv2.URL)
	out.Reset()
	if err := runCtl([]string{"jobs"}, &out); err != nil {
		t.Fatalf("ctl jobs empty: %v", err)
	}
	if !strings.Contains(out.String(), "no jobs") {
		t.Errorf("empty output = %q", out.String())
	}

	// server down → actionable error mentioning the address
	t.Setenv("AEGIS_ADDR", "http://127.0.0.1:1")
	t.Setenv("AEGIS_HTTP_TOKEN", "")
	out.Reset()
	if err := runCtl([]string{"jobs"}, &out); err == nil {
		t.Fatalf("ctl jobs down: want error, got nil (out=%q)", out.String())
	}
}
