package state

import (
	"context"
	"fmt"
	"time"
)

type PendingBudgetCall struct {
	CallID    string    `json:"call_id"`
	Day       string    `json:"day"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *StateStore) PendingBudgetCalls(ctx context.Context) ([]PendingBudgetCall, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("budget store unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT call_id,day,created_at FROM budget_calls WHERE status='pending' ORDER BY created_at,call_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]PendingBudgetCall, 0)
	for rows.Next() {
		var row PendingBudgetCall
		var at int64
		if err := rows.Scan(&row.CallID, &row.Day, &at); err != nil {
			return nil, err
		}
		row.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}
