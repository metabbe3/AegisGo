package router

import (
	"context"
	"strings"
	"testing"

	"aegisgo/internal/store"
)

// ReconcileSeeded tests run against a REAL :memory: store — the queryer
// interface returns *sql.Rows, so the store is the only honest fake
// (same pattern as rulesstore_test.go).

// seededAll loads rules via LoadRules the way first boot does.
func seededAll(t *testing.T) *store.Store {
	t.Helper()
	st := openRulesStore(t)
	if _, err := LoadRules(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestReconcileInsertsMissingOnly(t *testing.T) {
	st := seededAll(t)
	ctx := context.Background()

	// Simulate an OLDER prod DB: one seed rule never made it into the
	// table (rules added to Seeded() post-creation don't auto-seed).
	victim := seededDropName()
	if err := st.Exec(ctx, `DELETE FROM rules WHERE name = ?`, victim); err != nil {
		t.Fatal(err)
	}

	inserted, shadowed, err := ReconcileSeeded(ctx, st, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(inserted) != 1 || inserted[0] != victim {
		t.Errorf("inserted = %v, want exactly [%s]", inserted, victim)
	}
	if len(shadowed) != 0 {
		t.Errorf("shadowed = %v, want none", shadowed)
	}
	if got, want := countRules(ctx, t, st), len(Seeded()); got != want {
		t.Errorf("rules after reconcile = %d, want %d", got, want)
	}
}

func TestReconcileReportsShadowedButNeverReenables(t *testing.T) {
	st := seededAll(t)
	ctx := context.Background()

	// Operator demoted a rule — reconcile must report it shadowed and
	// never flip the row back on.
	const victim = "uptime"
	if err := st.Exec(ctx,
		`UPDATE rules SET state = ?, enabled = 0 WHERE name = ?`, RuleDemoted, victim); err != nil {
		t.Fatal(err)
	}

	inserted, shadowed, err := ReconcileSeeded(ctx, st, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(shadowed) != 1 || shadowed[0] != victim {
		t.Fatalf("shadowed = %v, want [%s]", shadowed, victim)
	}
	for _, n := range inserted {
		if n == victim {
			t.Errorf("demoted %s re-inserted — operator decision reverted", victim)
		}
	}
	var state string
	var enabled int
	if err := st.QueryRow(ctx,
		`SELECT state, enabled FROM rules WHERE name = ?`, victim).Scan(&state, &enabled); err != nil {
		t.Fatal(err)
	}
	if state != RuleDemoted || enabled != 0 {
		t.Errorf("victim row = (%s, %d), want (%s, 0) — untouched", state, enabled, RuleDemoted)
	}
}

func TestReconcileEmptyDBSeedsEverything(t *testing.T) {
	st := openRulesStore(t)
	ctx := context.Background()
	// A truly empty rules table after first boot (all rows deleted):
	// reconcile still refuses to duplicate anything on a second run.
	if _, err := LoadRules(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `DELETE FROM rules`); err != nil {
		t.Fatal(err)
	}
	inserted, _, err := ReconcileSeeded(ctx, st, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(inserted) != len(Seeded()) {
		t.Errorf("inserted %d, want %d (all seeds)", len(inserted), len(Seeded()))
	}
}

func TestReconcileInsertErrorPropagates(t *testing.T) {
	st := seededAll(t)
	ctx := context.Background()
	if err := st.Exec(ctx, `DROP TABLE rules`); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReconcileSeeded(ctx, st, nil)
	if err == nil || !strings.Contains(err.Error(), "reconcile seeded") {
		t.Fatalf("err = %v, want wrapped 'reconcile seeded' error", err)
	}
}

// seededDropName picks a deterministic seed name present in Seeded().
func seededDropName() string {
	// Stable as long as the seed set doesn't shrink; a rename failure
	// here is the honest signal to update the test.
	return Seeded()[len(Seeded())-1].Name
}
