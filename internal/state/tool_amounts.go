package state

import (
	"context"
	"errors"
	"fmt"
	"math/big"
)

// ErrNoStore is returned where a running total needs the durable store.
var ErrNoStore = errors.New("state store unavailable")

// RecordToolActionAmounts stores the capped argument values of one reserved
// tool action, so they count toward max_per_day once the action is allowed.
// Recording the same action twice keeps the first values.
func (s *StateStore) RecordToolActionAmounts(actionID, day string, amounts map[string]string) error {
	if s == nil || s.db == nil {
		return ErrNoStore
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for arg, amount := range amounts {
		if _, ok := new(big.Rat).SetString(amount); !ok {
			return fmt.Errorf("amount %q for %s is not a decimal number", amount, arg)
		}
		if _, err := tx.ExecContext(context.Background(),
			`INSERT OR IGNORE INTO tool_action_amounts (action_id, arg, amount, day) VALUES (?, ?, ?, ?)`,
			actionID, arg, amount, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToolArgTotal sums one argument over a tool's allowed actions on a UTC day.
// An action counts from the moment the gate allows it, consumed or not; a
// permit that is revoked or expires unconsumed stops counting, because it can
// no longer be executed.
func (s *StateStore) ToolArgTotal(tool, arg, day string) (*big.Rat, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT a.amount FROM tool_action_amounts a
		 JOIN tool_actions t ON t.action_id = a.action_id
		 WHERE t.tool = ? AND a.arg = ? AND a.day = ? AND t.decision = 'allow'`,
		tool, arg, day)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	total := new(big.Rat)
	for rows.Next() {
		var amount string
		if err := rows.Scan(&amount); err != nil {
			return nil, err
		}
		v, ok := new(big.Rat).SetString(amount)
		if !ok {
			return nil, fmt.Errorf("stored amount %q is not a decimal number", amount)
		}
		total.Add(total, v)
	}
	return total, rows.Err()
}
