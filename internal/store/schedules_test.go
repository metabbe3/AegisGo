package store

import (
	"context"
	"testing"
)

// TestSchedulesCRUD: Save → List → Delete round-trip on the v8 table,
// including the honest no-match delete signal.
func TestSchedulesCRUD(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	id1, err := st.SaveSchedule(ctx, 7, "/uptime", 1800000)
	if err != nil || id1 != 1 {
		t.Fatalf("save1 = %d %v", id1, err)
	}
	id2, err := st.SaveSchedule(ctx, 8, "/disk", 3600000)
	if err != nil || id2 != 2 {
		t.Fatalf("save2 = %d %v", id2, err)
	}
	rows, err := st.ListSchedules(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list = %v err=%v", rows, err)
	}
	if rows[0].ID != 1 || rows[0].ChatID != 7 || rows[0].Command != "/uptime" || rows[0].IntervalMs != 1800000 {
		t.Fatalf("row0 = %+v", rows[0])
	}
	ok, err := st.DeleteSchedule(ctx, id1)
	if err != nil || !ok {
		t.Fatalf("delete = %v %v (must match)", ok, err)
	}
	if ok, _ := st.DeleteSchedule(ctx, id1); ok {
		t.Fatal("second delete must report no-match")
	}
	rows, err = st.ListSchedules(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != id2 {
		t.Fatalf("post-delete rows = %v err=%v", rows, err)
	}
}

// TestMigrateV8IdempotentAcrossOpens: reopening a v8 DB skips migration
// and keeps rows (same guard shape as v7).
func TestMigrateV8IdempotentAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/v8.db"
	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := st1.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v < 8 {
		t.Fatalf("user_version = %d err=%v (want >=8)", v, err)
	}
	if _, err := st1.SaveSchedule(context.Background(), 7, "/uptime", 60000); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	rows, err := st2.ListSchedules(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v err=%v (data must survive reopen)", rows, err)
	}
}
