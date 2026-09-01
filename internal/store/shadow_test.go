package store

import (
	"context"
	"testing"
)

// openMem opens an in-memory store tied to the test's lifetime.
func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening :memory: store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustFlush drains the write queue: reads issued after it observe every
// prior async write (the single-writer batcher contract).
func mustFlush(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// mustExec runs one synchronous admin write through the batcher.
func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if err := s.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// insertRule seeds a router rule row directly (rule seeding is an admin
// write, so tests use Exec rather than a dedicated helper API).
func insertRule(t *testing.T, s *Store, name, origin, state string, enabled int) {
	t.Helper()
	mustExec(t, s,
		`INSERT INTO rules (name, pattern, tool, args_template, origin, enabled, created_ts, state)
		 VALUES (?,?,?,?,?,?,?,?)`,
		name, "^/"+name+"$", "read_csv", "{}", origin, enabled,
		"2026-09-01T00:00:00Z", state)
}

func TestRecordFallbackNullable(t *testing.T) {
	s := openMem(t)
	s.RecordFallback(FallbackEvent{
		TraceID:          "fb-1",
		NormalizedPrompt: "summarize <path>",
		RawPrompt:        "summarize data/2024.csv",
		TokensIn:         5,
		TokensOut:        7,
	})
	mustFlush(t, s)

	var tools, answerHash, model *string
	var tokensIn, tokensOut int
	err := s.QueryRow(context.Background(),
		`SELECT tools_used, answer_sha256, model, tokens_in, tokens_out
		 FROM fallback_events WHERE trace_id=?`, "fb-1",
	).Scan(&tools, &answerHash, &model, &tokensIn, &tokensOut)
	if err != nil {
		t.Fatal(err)
	}
	if tools != nil || answerHash != nil || model != nil {
		t.Errorf("empty tools/answer/model must persist as NULLs, got %v/%v/%v", tools, answerHash, model)
	}
	if tokensIn != 5 || tokensOut != 7 {
		t.Errorf("tokens = %d/%d, want 5/7", tokensIn, tokensOut)
	}
}

func TestShadowStreakBreaksOnDisagreement(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	record := func(agreed bool) {
		t.Helper()
		if err := s.RecordShadow(ctx, ShadowEvent{RuleName: "mined_1", TraceID: "tr", Agreed: agreed}); err != nil {
			t.Fatalf("RecordShadow(agreed=%v): %v", agreed, err)
		}
	}

	for i := 0; i < 3; i++ {
		record(true)
	}
	if got, err := s.ShadowStreak(ctx, "mined_1"); err != nil || got != 3 {
		t.Fatalf("after 3 agreements: streak=%d err=%v, want 3/nil", got, err)
	}

	// A disagreement resets the streak to zero — divergence always demotes.
	record(false)
	if got, err := s.ShadowStreak(ctx, "mined_1"); err != nil || got != 0 {
		t.Fatalf("after disagreement: streak=%d err=%v, want 0/nil", got, err)
	}

	for i := 0; i < 2; i++ {
		record(true)
	}
	if got, err := s.ShadowStreak(ctx, "mined_1"); err != nil || got != 2 {
		t.Fatalf("after 2 more agreements: streak=%d err=%v, want 2/nil", got, err)
	}
}

func TestShadowStreakEmpty(t *testing.T) {
	s := openMem(t)
	got, err := s.ShadowStreak(context.Background(), "mined_1")
	if err != nil || got != 0 {
		t.Errorf("ShadowStreak with no events = %d, %v; want 0, nil", got, err)
	}
}

func TestRecordShadowRoundTrip(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	events := []ShadowEvent{
		{RuleName: "mined_1", TraceID: "tr-1", Agreed: true, LLMTools: []string{"read_csv", "csv_stats"}},
		{RuleName: "mined_1", TraceID: "tr-2", Agreed: false},
	}
	for _, ev := range events {
		if err := s.RecordShadow(ctx, ev); err != nil {
			t.Fatalf("RecordShadow(%+v): %v", ev, err)
		}
	}

	rows, err := s.Query(ctx, `SELECT rule_name, trace_id, agreed, llm_tools FROM shadow_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type row struct {
		rule, trace string
		agreed      int
		tools       *string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rule, &r.trace, &r.agreed, &r.tools); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("shadow rows = %d, want 2", len(all))
	}
	if all[0].rule != "mined_1" || all[0].trace != "tr-1" || all[0].agreed != 1 {
		t.Errorf("first row = %+v", all[0])
	}
	if all[0].tools == nil || *all[0].tools != "read_csv,csv_stats" {
		t.Errorf("llm_tools = %v, want comma-joined list", all[0].tools)
	}
	if all[1].trace != "tr-2" || all[1].agreed != 0 {
		t.Errorf("second row = %+v", all[1])
	}
	if all[1].tools != nil {
		t.Errorf("empty LLMTools must persist as NULL, got %q", *all[1].tools)
	}
}

func TestSetRuleStateAndRuleStates(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	insertRule(t, s, "echo_shape", "mined", RuleStateShadow, 1)
	insertRule(t, s, "uptime", "seed", RuleStateActive, 1)

	if err := s.SetRuleState(ctx, "uptime", RuleStateDemoted, false); err != nil {
		t.Fatalf("SetRuleState: %v", err)
	}

	states, err := s.RuleStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("rules = %d, want 2", len(states))
	}
	// RuleStates orders by origin, name: "mined" sorts before "seed".
	want := []struct {
		name, state string
		enabled     bool
	}{
		{"echo_shape", RuleStateShadow, true},
		{"uptime", RuleStateDemoted, false},
	}
	for i, w := range want {
		got := states[i]
		if got["name"] != w.name || got["state"] != w.state || got["enabled"] != w.enabled {
			t.Errorf("states[%d] = %v, want %s/%s/enabled=%v", i, got, w.name, w.state, w.enabled)
		}
	}
	if states[1]["pattern"] != "^/uptime$" || states[1]["tool"] != "read_csv" || states[1]["origin"] != "seed" {
		t.Errorf("uptime row fields = %v", states[1])
	}
}

func TestNextMinedNameSkipsUsed(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		seed  func(t *testing.T, s *Store)
		want  string
		whyIt string
	}{
		{
			name:  "empty rules table",
			seed:  func(*testing.T, *Store) {},
			want:  "mined_1",
			whyIt: "first mined rule gets the base name",
		},
		{
			name: "two mined rules present",
			seed: func(t *testing.T, s *Store) {
				insertRule(t, s, "mined_1", "mined", RuleStateShadow, 1)
				insertRule(t, s, "mined_2", "mined", RuleStateActive, 1)
			},
			want:  "mined_3",
			whyIt: "counter starts past the mined-origin count",
		},
		{
			name: "seeded rule squats on mined_1",
			seed: func(t *testing.T, s *Store) {
				// The mined-origin count is zero here, so the first
				// candidate is mined_1 — the collision loop must skip it
				// because name existence, not origin, decides.
				insertRule(t, s, "mined_1", "seed", RuleStateActive, 1)
			},
			want:  "mined_2",
			whyIt: "collision loop skips existing names",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openMem(t)
			tc.seed(t, s)
			got, err := s.NextMinedName(ctx)
			if err != nil {
				t.Fatalf("NextMinedName: %v", err)
			}
			if got != tc.want {
				t.Errorf("NextMinedName() = %q, want %q (%s)", got, tc.want, tc.whyIt)
			}
		})
	}
}
