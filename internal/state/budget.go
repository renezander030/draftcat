package state

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// BudgetDay holds settled usage for a full UTC calendar date.
type BudgetDay struct {
	Tokens      int
	Cost        float64
	Calls       int
	CallMinutes int
	Unsettled   int
}

// UTCDay is the day key every daily total is kept under: the UTC calendar
// date of at, as YYYY-MM-DD.
func UTCDay(at time.Time) string { return at.UTC().Format("2006-01-02") }

func (s *StateStore) BudgetDay(ctx context.Context, at time.Time) (BudgetDay, error) {
	var d BudgetDay
	if s == nil || s.db == nil {
		return d, fmt.Errorf("budget store unavailable")
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT tokens FROM budget_days WHERE day=?),0), COALESCE((SELECT cost FROM budget_days WHERE day=?),0), COALESCE((SELECT calls FROM budget_days WHERE day=?),0), COALESCE((SELECT call_minutes FROM budget_days WHERE day=?),0), (SELECT COUNT(*) FROM budget_calls WHERE status='pending')`, UTCDay(at), UTCDay(at), UTCDay(at), UTCDay(at)).Scan(&d.Tokens, &d.Cost, &d.Calls, &d.CallMinutes, &d.Unsettled)
	if err == nil && (d.Tokens < 0 || d.Cost < 0 || math.IsNaN(d.Cost) || math.IsInf(d.Cost, 0)) {
		err = fmt.Errorf("invalid persisted budget usage")
	}
	return d, err
}

// BeginBudgetCall atomically admits one provider call across store handles.
// A pending call survives a crash; no subsequent call can assume it was free.
// Cost is a stop threshold on settled spend, not an estimated dollar ceiling.
// The label's pipeline caps apply the same two checks to the calls settled for
// that pipeline on the same UTC date, in the same statement.
func (s *StateStore) BeginBudgetCall(ctx context.Context, id string, at time.Time, requested, tokenLimit int, costLimit float64, label UsageLabel) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budget store unavailable")
	}
	if id == "" || requested < 0 || math.IsNaN(costLimit) || math.IsInf(costLimit, 0) || math.IsNaN(label.PipelineDayCost) || math.IsInf(label.PipelineDayCost, 0) {
		return fmt.Errorf("invalid budget admission")
	}
	day := UTCDay(at)
	result, err := s.db.ExecContext(ctx, `INSERT INTO budget_calls (call_id, day, status, created_at, model, pipeline)
 SELECT ?, ?, 'pending', ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM budget_calls WHERE status='pending')
 AND (? <= 0 OR COALESCE((SELECT tokens FROM budget_days WHERE day=?),0) <= ? - ?)
 AND (? <= 0 OR COALESCE((SELECT cost FROM budget_days WHERE day=?),0) < ?)
 AND (? <= 0 OR (SELECT COALESCE(SUM(tokens),0) FROM budget_calls WHERE day=? AND pipeline=? AND status='settled') <= ? - ?)
 AND (? <= 0 OR (SELECT COALESCE(SUM(cost),0) FROM budget_calls WHERE day=? AND pipeline=? AND status='settled') < ?)`,
		id, day, at.Unix(), label.Model, label.Pipeline,
		tokenLimit, day, tokenLimit, requested,
		costLimit, day, costLimit,
		label.PipelineDayTokens, day, label.Pipeline, label.PipelineDayTokens, requested,
		label.PipelineDayCost, day, label.Pipeline, label.PipelineDayCost)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("BUDGET_BLOCKED: daily limit reached or unresolved provider usage; reconcile the pending call before retrying")
	}
	return nil
}

// SettleBudgetCall charges an admitted call exactly once, including denied
// output. Reconciliation may use this same method after verifying actual usage.
func (s *StateStore) SettleBudgetCall(ctx context.Context, id string, tokens int, cost float64) error {
	if tokens < 0 || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return fmt.Errorf("invalid provider usage")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var day, status string
	if err := tx.QueryRowContext(ctx, `SELECT day,status FROM budget_calls WHERE call_id=?`, id).Scan(&day, &status); err != nil {
		return err
	}
	if status != "pending" {
		return fmt.Errorf("provider call is already settled")
	}
	if err := addBudgetUsageTx(ctx, tx, day, tokens, cost, 0, 0); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE budget_calls SET status='settled',tokens=?,cost=?,settled_at=? WHERE call_id=? AND status='pending'`, tokens, cost, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseBudgetCall closes a request known not to have produced billable usage.
func (s *StateStore) ReleaseBudgetCall(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE budget_calls SET status='rejected',settled_at=? WHERE call_id=? AND status='pending'`, time.Now().Unix(), id)
	return err
}

func (s *StateStore) AddBudgetUsage(ctx context.Context, at time.Time, tokens int, cost float64, calls, minutes int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := addBudgetUsageTx(ctx, tx, UTCDay(at), tokens, cost, calls, minutes); err != nil {
		return err
	}
	return tx.Commit()
}

func addBudgetUsageTx(ctx context.Context, tx *sql.Tx, day string, tokens int, cost float64, calls, minutes int) error {
	if tokens < 0 || cost < 0 || calls < 0 || minutes < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return fmt.Errorf("invalid budget usage")
	}
	var current BudgetDay
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT tokens FROM budget_days WHERE day=?),0), COALESCE((SELECT cost FROM budget_days WHERE day=?),0), COALESCE((SELECT calls FROM budget_days WHERE day=?),0), COALESCE((SELECT call_minutes FROM budget_days WHERE day=?),0)`, day, day, day, day).Scan(&current.Tokens, &current.Cost, &current.Calls, &current.CallMinutes)
	if err != nil {
		return err
	}
	maxInt := int(^uint(0) >> 1)
	if current.Tokens < 0 || current.Calls < 0 || current.CallMinutes < 0 || current.Cost < 0 || tokens > maxInt-current.Tokens || calls > maxInt-current.Calls || minutes > maxInt-current.CallMinutes || math.IsNaN(current.Cost+cost) || math.IsInf(current.Cost+cost, 0) {
		return fmt.Errorf("budget usage overflow or invalid stored totals")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_days (day,tokens,cost,calls,call_minutes) VALUES (?,?,?,?,?) ON CONFLICT(day) DO UPDATE SET tokens=excluded.tokens,cost=excluded.cost,calls=excluded.calls,call_minutes=excluded.call_minutes`, day, current.Tokens+tokens, current.Cost+cost, current.Calls+calls, current.CallMinutes+minutes)
	return err
}
