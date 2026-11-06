package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Voice call usage for budgets.per_day_calls and budgets.per_day_call_minutes.
//
// A call is counted once, when it is admitted, and its minutes once, when it
// ends; both land in budget_days, the ledger the token and cost caps use, on
// the UTC day the call was admitted. budget_voice_calls remembers each call
// by its session id, so a repeated webhook never counts a call twice and an
// end can find the day its call was counted on.

// VoiceCallCaps are the daily voice call caps; 0 = no cap.
type VoiceCallCaps struct {
	Calls   int
	Minutes int
}

// Capped reports whether either cap is set.
func (c VoiceCallCaps) Capped() bool { return c.Calls > 0 || c.Minutes > 0 }

// VoiceCapError is a call refused because a daily cap is reached.
type VoiceCapError struct {
	Cap   string // "per_day_calls" or "per_day_call_minutes"
	Limit int
	Used  int
	Day   string // UTC date, YYYY-MM-DD
}

func (e *VoiceCapError) Error() string {
	if e.Cap == "per_day_calls" {
		return fmt.Sprintf("BUDGET_BLOCKED: daily voice call limit %d reached (used: %d, UTC day %s)", e.Limit, e.Used, e.Day)
	}
	return fmt.Sprintf("BUDGET_BLOCKED: daily call-minute limit %d reached (used: %d, UTC day %s)", e.Limit, e.Used, e.Day)
}

func voiceCapReached(d BudgetDay, caps VoiceCallCaps, day string) error {
	if caps.Calls > 0 && d.Calls >= caps.Calls {
		return &VoiceCapError{Cap: "per_day_calls", Limit: caps.Calls, Used: d.Calls, Day: day}
	}
	if caps.Minutes > 0 && d.CallMinutes >= caps.Minutes {
		return &VoiceCapError{Cap: "per_day_call_minutes", Limit: caps.Minutes, Used: d.CallMinutes, Day: day}
	}
	return nil
}

// CheckVoiceCall reports whether a new call may start at at, without
// counting one: nil, a *VoiceCapError, or a ledger error.
func (s *StateStore) CheckVoiceCall(ctx context.Context, at time.Time, caps VoiceCallCaps) error {
	if !caps.Capped() {
		return nil
	}
	d, err := s.BudgetDay(ctx, at)
	if err != nil {
		return err
	}
	return voiceCapReached(d, caps, UTCDay(at))
}

// AdmitVoiceCall counts call id against the caps on the UTC day of at, in one
// transaction with the cap check. A call already counted is admitted again
// without a second count (counted = false). A refused call is remembered, so
// its end is not counted as a call; a later admission of the same id is
// checked again.
func (s *StateStore) AdmitVoiceCall(ctx context.Context, id string, at time.Time, caps VoiceCallCaps) (counted bool, err error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("budget store unavailable")
	}
	if id == "" {
		return false, fmt.Errorf("voice call id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	switch err := tx.QueryRowContext(ctx, `SELECT status FROM budget_voice_calls WHERE call_id=?`, id).Scan(&status); {
	case err == nil && status != "refused":
		return false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	day := UTCDay(at)
	d, err := voiceDayTx(ctx, tx, day)
	if err != nil {
		return false, err
	}
	if capErr := voiceCapReached(d, caps, day); capErr != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_voice_calls (call_id, day, status, admitted_at) VALUES (?, ?, 'refused', ?)
 ON CONFLICT(call_id) DO NOTHING`, id, day, at.Unix()); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, capErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_voice_calls (call_id, day, status, admitted_at) VALUES (?, ?, 'open', ?)
 ON CONFLICT(call_id) DO UPDATE SET day=excluded.day, status='open', admitted_at=excluded.admitted_at`, id, day, at.Unix()); err != nil {
		return false, err
	}
	if err := addBudgetUsageTx(ctx, tx, day, 0, 0, 1, 0); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EndVoiceCall records the minutes of call id once, on the day it was
// counted. A call never admitted (its start was missed, or it began before
// the caps were counted) is counted here, on the UTC day of at. The end of a
// refused call records nothing.
func (s *StateStore) EndVoiceCall(ctx context.Context, id string, at time.Time, minutes int) (counted bool, err error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("budget store unavailable")
	}
	if id == "" || minutes < 0 {
		return false, fmt.Errorf("invalid voice call end")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var day, status string
	switch err := tx.QueryRowContext(ctx, `SELECT day, status FROM budget_voice_calls WHERE call_id=?`, id).Scan(&day, &status); {
	case errors.Is(err, sql.ErrNoRows):
		day = UTCDay(at)
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_voice_calls (call_id, day, status, admitted_at, ended_at, minutes) VALUES (?, ?, 'ended', ?, ?, ?)`,
			id, day, at.Unix(), at.Unix(), minutes); err != nil {
			return false, err
		}
		if err := addBudgetUsageTx(ctx, tx, day, 0, 0, 1, minutes); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case status != "open":
		return false, nil
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE budget_voice_calls SET status='ended', ended_at=?, minutes=? WHERE call_id=? AND status='open'`,
			at.Unix(), minutes, id); err != nil {
			return false, err
		}
		if err := addBudgetUsageTx(ctx, tx, day, 0, 0, 0, minutes); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func voiceDayTx(ctx context.Context, tx *sql.Tx, day string) (BudgetDay, error) {
	var d BudgetDay
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT calls FROM budget_days WHERE day=?),0), COALESCE((SELECT call_minutes FROM budget_days WHERE day=?),0)`,
		day, day).Scan(&d.Calls, &d.CallMinutes)
	return d, err
}
