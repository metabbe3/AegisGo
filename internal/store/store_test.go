package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenMigrateAndAudit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	s.Audit(context.Background(), AuditEvent{
		TraceID:        "trace-1",
		Interface:      IFaceREST,
		DecisionSource: SourceRouter,
		RuleID:         "uptime",
		Prompt:         "/uptime",
		LatencyMS:      3,
	})
	s.Audit(context.Background(), AuditEvent{
		TraceID:        "trace-2",
		Interface:      IFaceCLI,
		DecisionSource: SourceLLM,
		Prompt:         "what happened to the numbers?",
		Model:          "llama3",
		TokensIn:       10,
		TokensOut:      20,
		LatencyMS:      900,
	})

	// Batched writes are async; force a sync write to flush ordering.
	if err := s.execSync(context.Background(),
		`INSERT INTO answers (trace_id, status, output, created_ts, expires_ts) VALUES ('x','done','o',?,?)`,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query(context.Background(),
		`SELECT trace_id, decision_source, rule_id, prompt_sha256, tokens_out FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type got struct {
		id, src, rule, phash string
		tokensOut            int
	}
	var all []got
	for rows.Next() {
		var g got
		var rule, phash *string
		if err := rows.Scan(&g.id, &g.src, &rule, &phash, &g.tokensOut); err != nil {
			t.Fatal(err)
		}
		if rule != nil {
			g.rule = *rule
		}
		if phash != nil {
			g.phash = *phash
		}
		all = append(all, g)
	}
	if len(all) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(all))
	}
	if all[0].src != SourceRouter || all[0].rule != "uptime" || all[0].phash == "" {
		t.Errorf("router row = %+v", all[0])
	}
	if all[1].src != SourceLLM || all[1].tokensOut != 20 {
		t.Errorf("llm row = %+v", all[1])
	}
}

func TestAnswerLifecycle(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.PutAnswer(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.GetAnswer(ctx, "t1")
	if err != nil || !ok || a.Status != AnswerPending {
		t.Fatalf("pending: %+v ok=%v err=%v", a, ok, err)
	}
	if err := s.CompleteAnswer(ctx, "t1", AnswerDone, "the answer"); err != nil {
		t.Fatal(err)
	}
	a, ok, err = s.GetAnswer(ctx, "t1")
	if err != nil || !ok || a.Output != "the answer" || a.Status != AnswerDone {
		t.Fatalf("done: %+v ok=%v err=%v", a, ok, err)
	}

	// Expired answers read as absent.
	if err := s.execSync(ctx, `UPDATE answers SET expires_ts=? WHERE trace_id='t1'`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetAnswer(ctx, "t1"); ok {
		t.Error("expired answer should be absent")
	}
}

func TestNormalizePrompt(t *testing.T) {
	cases := map[string]string{
		"summarize data/2024.csv":  "summarize <path>",
		"summarize data/2025.csv":  "summarize <path>",
		"total for 2024 and 2025":  "total for <n> and <n>",
		"average of 3.5 and 1,000": "average of <n> and <n>",
		`echo "hello world" again`: "echo <q> again",
		`echo "data/1.csv" again`:  "echo <q> again",
		"how many rows":            "how many rows",
		"":                         "",
	}
	for in, want := range cases {
		if got := NormalizePrompt(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOpenUncreatablePathFails checks the fail-fast contract for bad paths:
// Open must return the ping error (and close the pool) instead of handing
// back a store that fails on first use.
func TestOpenUncreatablePathFails(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "no-such-dir", "x.db"))
	if err == nil {
		t.Fatal("Open with a missing parent directory should fail")
	}
}

// TestGetAnswerCorruptExpiry covers the guarded parse: a malformed
// expires_ts must surface as an error, never as a phantom answer.
func TestGetAnswerCorruptExpiry(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	mustExec(t, s,
		`INSERT INTO answers (trace_id, status, output, created_ts, expires_ts)
		 VALUES ('bad','done','o','2026-09-01T00:00:00Z','not-a-timestamp')`)
	if _, ok, err := s.GetAnswer(ctx, "bad"); err == nil || !strings.Contains(err.Error(), "parsing expires_ts") {
		t.Errorf("GetAnswer(corrupt expiry) = ok=%v err=%v, want parsing expires_ts error", ok, err)
	}
}

// TestConcurrentAuditNoBusy is the go/no-go gate for the single-writer
// batcher: hammering audit writes from many goroutines must not surface
// SQLITE_BUSY (or any) errors and must persist every row.
func TestConcurrentAuditNoBusy(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const workers, perWorker = 50, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				s.Audit(context.Background(), AuditEvent{
					TraceID:        "t",
					Interface:      IFaceREST,
					DecisionSource: SourceRouter,
					Prompt:         "/uptime",
				})
			}
		}(w)
	}
	wg.Wait()

	// Flush: one sync write waits behind the queue.
	if err := s.execSync(context.Background(),
		`INSERT INTO answers (trace_id, status, output, created_ts, expires_ts) VALUES ('flush','done','','','')`,
	); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query(context.Background(), `SELECT COUNT(*) FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	if !rows.Next() || rows.Scan(&n) != nil {
		t.Fatal("count query failed")
	}
	if n != workers*perWorker {
		t.Errorf("audit rows = %d, want %d (lost writes)", n, workers*perWorker)
	}
}
