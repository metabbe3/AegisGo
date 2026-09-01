package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Answer statuses for the async answer store.
const (
	AnswerPending = "pending"
	AnswerDone    = "done"
	AnswerError   = "error"
)

// AnswerTTL is how long a completed answer stays pollable.
const AnswerTTL = 15 * time.Minute

// Answer is one async result, keyed by trace_id.
type Answer struct {
	TraceID string `json:"trace_id"`
	Status  string `json:"status"`
	Output  string `json:"output"`
}

// PutAnswer registers a pending answer (the 202 ack's promise).
func (s *Store) PutAnswer(ctx context.Context, traceID string) error {
	now := time.Now().UTC()
	return s.execSync(ctx,
		`INSERT OR REPLACE INTO answers (trace_id, status, output, created_ts, expires_ts) VALUES (?,?,?,?,?)`,
		traceID, AnswerPending, "", now.Format(time.RFC3339Nano), now.Add(AnswerTTL).Format(time.RFC3339Nano),
	)
}

// CompleteAnswer stores the final output for a trace.
func (s *Store) CompleteAnswer(ctx context.Context, traceID, status, output string) error {
	now := time.Now().UTC()
	return s.execSync(ctx,
		`UPDATE answers SET status=?, output=?, expires_ts=? WHERE trace_id=?`,
		status, output, now.Add(AnswerTTL).Format(time.RFC3339Nano), traceID,
	)
}

// GetAnswer returns the answer for a trace; expired rows read as absent and
// are lazily deleted.
func (s *Store) GetAnswer(ctx context.Context, traceID string) (Answer, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT status, output, expires_ts FROM answers WHERE trace_id=?`, traceID)
	var status, output, expires string
	if err := row.Scan(&status, &output, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Answer{}, false, nil
		}
		return Answer{}, false, err
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return Answer{}, false, fmt.Errorf("parsing expires_ts: %w", err)
	}
	if time.Now().UTC().After(exp) {
		s.exec(`DELETE FROM answers WHERE trace_id=?`, traceID)
		return Answer{}, false, nil
	}
	return Answer{TraceID: traceID, Status: status, Output: output}, true, nil
}
