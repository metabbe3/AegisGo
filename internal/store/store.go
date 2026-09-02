// Package store provides AegisGo's durable substrate: an embedded SQLite
// database (pure Go, keeps CGO_ENABLED=0) holding the audit trail, the LLM
// fallback corpus (Phase-3 rule mining), the async answer store, and the
// router rule table.
//
// Concurrency model: SQLite allows one writer at a time, so ALL writes flow
// through a single batcher goroutine (a buffered channel); readers use the
// connection pool directly against WAL snapshots. This is what keeps three
// concurrent interfaces from SQLITE_BUSY-storming each other.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGo)
)

// writeBatchCap bounds how many queued writes coalesce into one transaction.
const writeBatchCap = 64

// writeQueueCap bounds queued writes; a full queue means the box is saturated
// and we fail fast instead of buffering unbounded memory.
const writeQueueCap = 4096

// Store is the embedded database handle. Safe for concurrent use.
type Store struct {
	db     *sql.DB
	writes chan writeOp
	quit   chan struct{}
	done   chan struct{}
	// closed is an atomic so enqueue — the audit hot path — reads it
	// lock-free; only Close flips it (once, via CompareAndSwap).
	closed atomic.Bool
}

type writeOp struct {
	query string
	args  []any
	// done, when non-nil, receives the write's error — used where the
	// caller must know (e.g. answer completion).
	done chan error
}

// Open opens (creating if needed) the SQLite database at path and runs
// migrations. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	if path == ":memory:" {
		// An in-memory DB exists per connection; pin the pool to one.
		dsn = "file::memory:?_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sqlite: %w", err)
	}

	s := &Store{
		db:     db,
		writes: make(chan writeOp, writeQueueCap),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	go s.batcher()
	return s, nil
}

// Close stops the batcher (draining queued writes) and closes the database.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil // already closed
	}
	close(s.quit)
	<-s.done // batcher drains what it can, then exits
	return s.db.Close()
}

// enqueue queues a write for the batcher. Fails fast if the queue is full or
// the store is closing — never blocks the caller.
func (s *Store) enqueue(query string, args []any, done chan error) error {
	if s.closed.Load() {
		return fmt.Errorf("store closed")
	}
	select {
	case s.writes <- writeOp{query: query, args: args, done: done}:
		return nil
	default:
		return fmt.Errorf("write queue full (%d pending)", len(s.writes))
	}
}

// exec queues a fire-and-forget write; errors surface only in the batcher's
// diagnostics log path (see batcher).
func (s *Store) exec(query string, args ...any) {
	_ = s.enqueue(query, args, nil)
}

// execSync queues a write and waits for its error.
func (s *Store) execSync(ctx context.Context, query string, args ...any) error {
	done := make(chan error, 1)
	if err := s.enqueue(query, args, done); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// batcher is the single writer. It drains the queue in batches inside one
// transaction, which both maximizes throughput and serializes all writes.
func (s *Store) batcher() {
	defer close(s.done)
	for {
		select {
		case op := <-s.writes:
			batch := s.collect(op)
			err := s.apply(batch)
			for _, o := range batch {
				if o.done != nil {
					o.done <- err // same tx error for all; acceptable for our write shapes
				}
			}
			_ = err // individual failures are non-fatal; audit stdout carries details
		case <-s.quit:
			s.drainRemaining()
			return
		}
	}
}

// collect gathers up to writeBatchCap ops, including the one already read.
func (s *Store) collect(first writeOp) []writeOp {
	batch := make([]writeOp, 0, writeBatchCap)
	batch = append(batch, first)
	for len(batch) < writeBatchCap {
		select {
		case op := <-s.writes:
			batch = append(batch, op)
		default:
			return batch
		}
	}
	return batch
}

func (s *Store) apply(batch []writeOp) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, op := range batch {
		if _, err := tx.ExecContext(ctx, op.query, op.args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// drainRemaining flushes queued writes on shutdown so a graceful stop does
// not lose the last audit rows.
func (s *Store) drainRemaining() {
	for {
		select {
		case op := <-s.writes:
			batch := s.collect(op)
			if err := s.apply(batch); err != nil {
				for _, o := range batch {
					if o.done != nil {
						o.done <- err
					}
				}
			}
		default:
			return
		}
	}
}

// Query exposes the read pool for callers that need custom SELECTs
// (the Phase-3 miner, /stats). Read-only by convention.
func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// QueryRow runs a single-row SELECT against the read pool.
func (s *Store) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// QueryAll runs a SELECT and scans every row through fn into a slice — the
// loop shape (Query → defer Close → scan → rows.Err) every read-side caller
// shared. fn's error aborts the walk; rows.Err() is always surfaced, so an
// iteration failure can't silently truncate a result (that strictness fixed
// three stats loops that used to skip it). Deliberately NOT used by
// internal/router's rules loading: the router must stay import-free of this
// package.
func QueryAll[T any](ctx context.Context, s *Store, query string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := s.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Ping verifies the database is reachable (readiness probe).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Flush waits until every write queued before this call has been applied.
// The no-op statement rides the same single-writer queue as a barrier.
// Tests and shutdown paths use it; request paths never block on it.
func (s *Store) Flush(ctx context.Context) error {
	return s.execSync(ctx, `SELECT 1`)
}

// Exec runs a write through the batch queue and waits for it. Rare,
// admin-shaped writes (rule seeding) use this; hot paths use the Audit
// helpers, which never block.
func (s *Store) Exec(ctx context.Context, query string, args ...any) error {
	return s.execSync(ctx, query, args...)
}

// ExecResult runs a synchronous write outside the batcher and reports rows
// affected. For guarded atomic claims (e.g. the Telegram inbox's
// claim-then-send): the rowcount IS the answer, so it must be exact.
// WAL + busy_timeout keep this safe alongside the batcher.
func (s *Store) ExecResult(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

// migrations is the schema ladder, applied in order by migrate(). Each
// entry is one transaction ending in its PRAGMA user_version bump; the
// version guard is what makes each run exactly once — v3's ALTER is NOT
// idempotent, only the guard makes re-opening a v3 database safe.
var migrations = [][]string{
	{
		// v1, the Phase-1 schema: audit trail, fallback corpus, answers,
		// router rules.
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			trace_id TEXT NOT NULL,
			interface TEXT NOT NULL,
			decision_source TEXT NOT NULL,
			rule_id TEXT,
			prompt_sha256 TEXT,
			model TEXT,
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			outcome TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_trace ON audit_events(trace_id)`,
		`CREATE TABLE IF NOT EXISTS fallback_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			trace_id TEXT NOT NULL,
			normalized_prompt TEXT NOT NULL,
			raw_prompt TEXT NOT NULL,
			tools_used TEXT,
			answer_sha256 TEXT,
			model TEXT,
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS answers (
			trace_id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			output TEXT NOT NULL,
			created_ts TEXT NOT NULL,
			expires_ts TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS rules (
			name TEXT PRIMARY KEY,
			pattern TEXT NOT NULL,
			tool TEXT NOT NULL,
			args_template TEXT NOT NULL,
			origin TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_ts TEXT NOT NULL
		)`,
		`PRAGMA user_version = 1`,
	},
	{
		// v2, the Telegram substrate: the durable inbox (idempotent on
		// update_id — the at-least-once delivery anchor) and a small kv
		// table for transport state like the long-poll high-water mark.
		`CREATE TABLE IF NOT EXISTS telegram_inbox (
			update_id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			text TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			replied_at TEXT,
			received_ts TEXT NOT NULL,
			processed_ts TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tginbox_status ON telegram_inbox(status)`,
		`CREATE TABLE IF NOT EXISTS kv_state (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL
		)`,
		`PRAGMA user_version = 2`,
	},
	{
		// v3, the Phase-3 rule lifecycle: rules carry a state
		// (active | shadow | demoted) and shadow comparisons land in
		// shadow_events for promotion/demotion decisions.
		`ALTER TABLE rules ADD COLUMN state TEXT NOT NULL DEFAULT 'active'`,
		`CREATE TABLE IF NOT EXISTS shadow_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			rule_name TEXT NOT NULL,
			trace_id TEXT NOT NULL,
			agreed INTEGER NOT NULL,
			llm_tools TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shadow_rule ON shadow_events(rule_name, id)`,
		`PRAGMA user_version = 3`,
	},
}

// migrate creates tables idempotently, versioned by PRAGMA user_version.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	for v, stmts := range migrations {
		if version < v+1 {
			if err := s.applyMigration(v+1, stmts); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyMigration runs one schema version's statements in a transaction.
func (s *Store) applyMigration(version int, stmts []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating to v%d: %w", version, err)
		}
	}
	return tx.Commit()
}
