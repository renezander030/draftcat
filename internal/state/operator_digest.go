package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The operator digest's reads and its one row of state: the date it was last
// handled for, and the failed tool outcomes it reports.

// DigestLastDay is the local date the operator digest was last evaluated for.
func (s *StateStore) DigestLastDay() (string, error) {
	if s == nil || s.db == nil {
		return "", ErrNoStore
	}
	var day string
	err := s.db.QueryRowContext(context.Background(), `SELECT last_day FROM operator_digest WHERE id=1`).Scan(&day)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return day, err
}

// SetDigestLastDay records the date the digest was handled for.
func (s *StateStore) SetDigestLastDay(day string) error {
	if s == nil || s.db == nil {
		return ErrNoStore
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO operator_digest (id, last_day) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET last_day=excluded.last_day`, day)
	return err
}

// FailedToolOutcomesSince counts callers' "failed" execution reports
// completed at or after since.
func (s *StateStore) FailedToolOutcomesSince(since time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrNoStore
	}
	var n int
	err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM tool_execution_outcomes WHERE status='failed' AND completed_at>=?`, since.Unix()).Scan(&n)
	return n, err
}
