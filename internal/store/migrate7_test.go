package store

import (
	"context"
	"testing"
)

// Opening a store twice on the same file: the second open must see v7
// already applied and skip re-migration (the version guard branch).
func TestMigrateV7IdempotentAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/v7.db"

	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := st1.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 7 {
		t.Fatalf("user_version = %d, want >=7 after first open", v)
	}
	// table usable
	if err := st1.Exec(context.Background(),
		`INSERT INTO log_watches (name, path, pattern) VALUES ('t','/t','p')`); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	st2, err := Open(path) // second open: version=7, migration skipped
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	var n int
	if err := st2.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM log_watches`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err=%v (data must survive reopen)", n, err)
	}
}
