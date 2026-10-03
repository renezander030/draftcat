package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ToolOutcome is a caller's attestation about one consumed action. It is
// separate from the permit: reporting a result never authorizes execution.
type ToolOutcome struct {
	ActionID    string
	BindingHash string
	Status      string
	ResultHash  string
	CompletedAt time.Time
}

func toolActionTx(tx *sql.Tx, id string) (ToolAction, error) {
	return scanToolAction(tx.QueryRowContext(context.Background(), `SELECT `+toolActionColumns+` FROM tool_actions WHERE action_id=?`, id).Scan)
}

func appendToolReceipt(tx *sql.Tx, e ApprovalEnvelope) error {
	_, err := tx.ExecContext(context.Background(), `INSERT INTO action_approvals
	 (pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got,
	  nonce, signature, run_id, policy, receipt_version, receipt_id, action_id, policy_hash,
	  binding_hash, expires_at, lifecycle)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 2, ?, ?, ?, ?, ?, ?)`,
		e.Pipeline, e.Step, e.DecidedAt.Unix(), e.Decision, e.OperatorID, e.PayloadHash,
		e.QuorumN, e.QuorumGot, e.Nonce, e.Signature, e.RunID, e.Policy, e.ReceiptID,
		e.ActionID, e.PolicyHash, e.BindingHash, e.ExpiresAt.Unix(), e.Lifecycle)
	return err
}

// DecideToolActionWithReceipt commits the conditional decision and its audit
// receipt together. A revoke or expiry that won first cannot be overwritten.
func (s *StateStore) DecideToolActionWithReceipt(id, status, decision, reason, by string, at time.Time, e ApprovalEnvelope) (ToolAction, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, fmt.Errorf("state store unavailable")
	}
	if status != "allowed" && status != "denied" {
		return ToolAction{}, fmt.Errorf("invalid tool decision state")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return ToolAction{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Persisted expiries have second precision. Exactly at expiration is invalid.
	if _, err := tx.ExecContext(context.Background(), `UPDATE tool_actions SET status='expired', decision='deny', reason='permit expired before consumption', updated_at=? WHERE action_id=? AND status='pending' AND expires_at<=?`, at.Unix(), id, at.Unix()); err != nil {
		return ToolAction{}, err
	}
	res, err := tx.ExecContext(context.Background(), `UPDATE tool_actions SET status=?,decision=?,reason=?,decided_by=?,updated_at=? WHERE action_id=? AND status='pending'`, status, decision, reason, by, at.Unix(), id)
	if err != nil {
		return ToolAction{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ToolAction{}, err
	}
	if n == 1 {
		if err := appendToolReceipt(tx, e); err != nil {
			return ToolAction{}, err
		}
	}
	a, err := toolActionTx(tx, id)
	if err != nil {
		return ToolAction{}, err
	}
	if err := tx.Commit(); err != nil {
		return ToolAction{}, err
	}
	return a, nil
}

// ConsumeToolActionWithReceipt spends a permit and appends the consumption
// receipt atomically; audit failure leaves the permit unspent.
func (s *StateStore) ConsumeToolActionWithReceipt(id, binding string, at time.Time, e ApprovalEnvelope) (ToolAction, bool, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, false, fmt.Errorf("state store unavailable")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return ToolAction{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(context.Background(), `UPDATE tool_actions SET status='consumed',consumed_at=?,updated_at=? WHERE action_id=? AND binding_hash=? AND status='allowed' AND expires_at>?`, at.Unix(), at.Unix(), id, binding, at.Unix())
	if err != nil {
		return ToolAction{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ToolAction{}, false, err
	}
	if n == 1 {
		if err := appendToolReceipt(tx, e); err != nil {
			return ToolAction{}, false, err
		}
	}
	a, err := toolActionTx(tx, id)
	if err != nil {
		return ToolAction{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ToolAction{}, false, err
	}
	return a, n == 1, nil
}

// NormalizeToolAction closes invalid unconsumed permits. Current policy and
// expiry apply equally to live tickets and records recovered after restart.
func (s *StateStore) NormalizeToolAction(id, policy string, at time.Time) (ToolAction, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, fmt.Errorf("state store unavailable")
	}
	_, err := s.db.ExecContext(context.Background(), `UPDATE tool_actions SET status='expired',decision='deny',reason=CASE WHEN policy_hash<>? THEN 'policy changed; request a new approval' ELSE 'permit expired before consumption' END,updated_at=? WHERE action_id=? AND status IN ('pending','allowed') AND (policy_hash<>? OR expires_at<=?)`, policy, at.Unix(), id, policy, at.Unix())
	if err != nil {
		return ToolAction{}, err
	}
	return s.ToolAction(id)
}

// CancelToolAction revokes only pending or allowed permits with this exact
// binding. The database serializes it against consumption. Matching retries
// of an already revoked action succeed; consumed actions cannot be revoked.
func (s *StateStore) CancelToolAction(id, binding string, at time.Time) (ToolAction, bool, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, false, fmt.Errorf("state store unavailable")
	}
	_, err := s.db.ExecContext(context.Background(), `UPDATE tool_actions SET status='revoked',decision='deny',reason='action revoked before consumption',decided_by='caller',updated_at=? WHERE action_id=? AND binding_hash=? AND status IN ('pending','allowed')`, at.Unix(), id, binding)
	if err != nil {
		return ToolAction{}, false, err
	}
	a, err := s.ToolAction(id)
	return a, err == nil && a.BindingHash == binding && a.Status == "revoked", err
}

func (s *StateStore) ToolOutcome(id string) (ToolOutcome, error) {
	if s == nil || s.db == nil {
		return ToolOutcome{}, fmt.Errorf("state store unavailable")
	}
	var o ToolOutcome
	var at int64
	err := s.db.QueryRowContext(context.Background(), `SELECT action_id,binding_hash,status,result_hash,completed_at FROM tool_execution_outcomes WHERE action_id=?`, id).Scan(&o.ActionID, &o.BindingHash, &o.Status, &o.ResultHash, &at)
	if err == nil {
		o.CompletedAt = time.Unix(at, 0)
	}
	return o, err
}

// CompleteToolAction records a single immutable caller-reported result after
// consumption. Same-outcome retries succeed; conflicting reports do not.
func (s *StateStore) CompleteToolAction(o ToolOutcome) (ToolOutcome, bool, error) {
	if s == nil || s.db == nil {
		return ToolOutcome{}, false, fmt.Errorf("state store unavailable")
	}
	if o.Status != "succeeded" && o.Status != "failed" {
		return ToolOutcome{}, false, fmt.Errorf("invalid execution status")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return ToolOutcome{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := toolActionTx(tx, o.ActionID)
	if err != nil {
		return ToolOutcome{}, false, err
	}
	if a.Status != "consumed" || a.BindingHash != o.BindingHash {
		return ToolOutcome{}, false, nil
	}
	_, err = tx.ExecContext(context.Background(), `INSERT OR IGNORE INTO tool_execution_outcomes(action_id,binding_hash,status,result_hash,completed_at) VALUES(?,?,?,?,?)`, o.ActionID, o.BindingHash, o.Status, o.ResultHash, o.CompletedAt.Unix())
	if err != nil {
		return ToolOutcome{}, false, err
	}
	var saved ToolOutcome
	var at int64
	err = tx.QueryRowContext(context.Background(), `SELECT action_id,binding_hash,status,result_hash,completed_at FROM tool_execution_outcomes WHERE action_id=?`, o.ActionID).Scan(&saved.ActionID, &saved.BindingHash, &saved.Status, &saved.ResultHash, &at)
	if err != nil {
		return ToolOutcome{}, false, err
	}
	saved.CompletedAt = time.Unix(at, 0)
	if err := tx.Commit(); err != nil {
		return ToolOutcome{}, false, err
	}
	return saved, saved.BindingHash == o.BindingHash && saved.Status == o.Status && saved.ResultHash == o.ResultHash, nil
}
