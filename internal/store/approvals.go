package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HITL approvals (blueprint §K4, ADR-0003). An approval request is created
// when an L2-tier action wants to run: the caller stores a serialized copy
// of the action (opaque here), gets an id, and WAITS. A human resolves it
// via approve/deny. States: pending → approved | denied | expired.
// The store never executes anything — it is the durable ledger only; the
// executor re-validates the action against policy at run time (fail-closed).

// Approval is one HITL gate row. Timestamps are RFC3339 strings, matching
// the store's TEXT-column convention (audit, answers).
type Approval struct {
	ID        int64
	CreatedAt string
	Kind      string // e.g. "system_command"
	Payload   string // serialized action (JSON), opaque to the store
	Reason    string // why L2, for the human
	State     string // pending | approved | denied | expired
	DecidedBy string // approver identity ("telegram:<id>", "cli")
	DecidedAt string // "" while pending
}

// CreateApproval enqueues a pending approval row and returns its id.
// Writes go through the single-writer batcher (rule #5) via execSync.
func (s *Store) CreateApproval(ctx context.Context, kind, payload, reason string) (int64, error) {
	// INSERT ... RETURNING keeps the write on one pooled connection and
	// hands back the id without racing the single-writer batcher (an
	// enqueued write can't return LastInsertId, and :memory: SQLite has
	// per-connection databases — a second pooled connection would not see
	// an unflushed batcher write).
	var id int64
	err := s.QueryRow(ctx, `INSERT INTO approvals (created_at, kind, payload, reason) VALUES (?, ?, ?, ?) RETURNING id`,
		time.Now().UTC().Format(time.RFC3339Nano), kind, payload, reason).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create approval: %w", err)
	}
	return id, nil
}

// GetApproval loads one row by id.
func (s *Store) GetApproval(ctx context.Context, id int64) (Approval, bool, error) {
	var a Approval
	err := s.QueryRow(ctx, `SELECT id, created_at, kind, payload, reason, state, COALESCE(decided_by,''), COALESCE(decided_at,'')
		FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.CreatedAt, &a.Kind, &a.Payload, &a.Reason, &a.State, &a.DecidedBy, &a.DecidedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Approval{}, false, nil
		}
		return Approval{}, false, fmt.Errorf("get approval %d: %w", id, err)
	}
	return a, true, nil
}

// DecideApproval atomically transitions a PENDING row to approved/denied.
// Only a pending row can be decided — double-decide and decide-after-expiry
// are rejected (idempotence is the caller's visible signal).
func (s *Store) DecideApproval(ctx context.Context, id int64, state, decidedBy string) (bool, error) {
	if state != "approved" && state != "denied" {
		return false, fmt.Errorf("decide approval: state must be approved|denied, got %q", state)
	}
	res, err := s.ExecResult(ctx, `UPDATE approvals
		SET state = ?, decided_by = ?, decided_at = ? WHERE id = ? AND state = 'pending'`,
		state, decidedBy, time.Now().UTC(), id)
	if err != nil {
		return false, fmt.Errorf("decide approval %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide approval %d: rows affected: %w", id, err)
	}
	return n == 1, nil // false = already decided/expired (idempotent no-op)
}

// ExpireApprovals flips pending rows older than ttl to expired (sweeper).
// Returns the number expired.
func (s *Store) ExpireApprovals(ctx context.Context, ttl time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-ttl)
	res, err := s.ExecResult(ctx, `UPDATE approvals SET state='expired' WHERE state='pending' AND created_at < ?`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("expire approvals: %w", err)
	}
	return res.RowsAffected()
}

// PendingApprovals lists pending rows oldest-first, capped (rule #10).
func (s *Store) PendingApprovals(ctx context.Context, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Query(ctx, `SELECT id, created_at, kind, payload, reason, state, COALESCE(decided_by,''), COALESCE(decided_at,'')
		FROM approvals WHERE state='pending' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending approvals: %w", err)
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Kind, &a.Payload, &a.Reason, &a.State, &a.DecidedBy, &a.DecidedAt); err != nil {
			return nil, fmt.Errorf("pending approvals scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// migrateV4 adds the approvals table (HITL ledger, ADR-0003).
func (s *Store) migrateV4() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS approvals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'pending',
			decided_by TEXT,
			decided_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_state ON approvals(state, id)`,
		`PRAGMA user_version = 4`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating to v4: %w", err)
		}
	}
	return tx.Commit()
}

// migrateV5 adds callback columns to the telegram inbox (inline keyboard
// presses carry data instead of text).
func (s *Store) migrateV5() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmts := []string{
		`ALTER TABLE telegram_inbox ADD COLUMN cb_id TEXT`,
		`ALTER TABLE telegram_inbox ADD COLUMN cb_data TEXT`,
		`PRAGMA user_version = 5`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating to v5: %w", err)
		}
	}
	return tx.Commit()
}

// RecentDecisions lists the latest decided rows (state != pending),
// newest-first, capped (rule #10) — backs the /history command.
func (s *Store) RecentDecisions(ctx context.Context, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.Query(ctx, `SELECT id, created_at, kind, payload, reason,
		state, COALESCE(decided_by,''), COALESCE(decided_at,'')
		FROM approvals WHERE state != 'pending'
		ORDER BY decided_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent decisions: %w", err)
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Kind, &a.Payload, &a.Reason,
			&a.State, &a.DecidedBy, &a.DecidedAt); err != nil {
			return nil, fmt.Errorf("recent decisions scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
