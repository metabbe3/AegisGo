package store

import (
	"context"
	"fmt"
)

// migrateV8 adds the scheduled_jobs table: the Telegram scheduler becomes
// durable — registered /every jobs survive restarts and nightly deploys
// (before this, schedules lived only in process memory and were silently
// wiped on every restart, and Scheduler.Start was never even called).
func (s *Store) migrateV8() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS scheduled_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		command TEXT NOT NULL,
		interval_ms INTEGER NOT NULL,
		created_ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`); err != nil {
		tx.Rollback()
		return fmt.Errorf("migrating to v8: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 8`); err != nil {
		tx.Rollback()
		return fmt.Errorf("migrating to v8: %w", err)
	}
	return tx.Commit()
}

// SchedJob is one durable scheduled command row.
type SchedJob struct {
	ID         int64
	ChatID     int64
	Command    string
	IntervalMs int64
}

// SaveSchedule persists a schedule row and returns its new id.
func (s *Store) SaveSchedule(ctx context.Context, chatID int64, command string, intervalMs int64) (int64, error) {
	res, err := s.ExecResult(ctx, `INSERT INTO scheduled_jobs
		(chat_id, command, interval_ms) VALUES (?, ?, ?)`,
		chatID, command, intervalMs)
	if err != nil {
		return 0, fmt.Errorf("save schedule: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("save schedule id: %w", err)
	}
	return id, nil
}

// DeleteSchedule removes a schedule row by id. Returns false when no row
// matched (the honest no-op signal, same contract as DecideApproval).
func (s *Store) DeleteSchedule(ctx context.Context, id int64) (bool, error) {
	res, err := s.ExecResult(ctx, `DELETE FROM scheduled_jobs WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete schedule %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete schedule %d: rows: %w", id, err)
	}
	return n > 0, nil
}

// ListSchedules returns all schedule rows, oldest first (insert order).
func (s *Store) ListSchedules(ctx context.Context) ([]SchedJob, error) {
	rows, err := s.Query(ctx, `SELECT id, chat_id, command, interval_ms
		FROM scheduled_jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	var out []SchedJob
	for rows.Next() {
		var j SchedJob
		if err := rows.Scan(&j.ID, &j.ChatID, &j.Command, &j.IntervalMs); err != nil {
			return nil, fmt.Errorf("list schedules scan: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
