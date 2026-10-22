package state

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UsageShare is the settled model usage of one model or pipeline on a day.
type UsageShare struct {
	Name   string  `json:"name"`
	Calls  int     `json:"calls"`
	Tokens int     `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// UsageLabel attributes an admitted model call to the provider model and the
// pipeline it serves. PipelineDayTokens and PipelineDayCost are that
// pipeline's own daily caps, checked at admission; 0 = no pipeline cap.
type UsageLabel struct {
	Model             string
	Pipeline          string
	PipelineDayTokens int
	PipelineDayCost   float64
}

// PipelineBudgetDay returns the settled model usage of one pipeline on the UTC
// date of at. A store the engine has not yet migrated reports none.
func (s *StateStore) PipelineBudgetDay(ctx context.Context, at time.Time, pipeline string) (UsageShare, error) {
	u := UsageShare{Name: pipeline}
	if s == nil || s.db == nil {
		return u, fmt.Errorf("budget store unavailable")
	}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(tokens),0), COALESCE(SUM(cost),0)
		FROM budget_calls WHERE day=? AND pipeline=? AND status='settled'`, UTCDay(at), pipeline).Scan(&u.Calls, &u.Tokens, &u.Cost)
	if err != nil && strings.Contains(err.Error(), "no such column") {
		return u, nil
	}
	return u, err
}

// BudgetUsageBy splits a day's settled model calls by "model" or "pipeline",
// largest cost first. Calls settled before attribution existed have an empty
// name. A store the engine has not yet migrated reports no split.
func (s *StateStore) BudgetUsageBy(ctx context.Context, at time.Time, by string) ([]UsageShare, error) {
	if by != "model" && by != "pipeline" {
		return nil, fmt.Errorf("usage split by %q", by)
	}
	// #nosec G202 -- by is one of two fixed column names.
	rows, err := s.db.QueryContext(ctx, `SELECT `+by+`, COUNT(*), COALESCE(SUM(tokens),0), COALESCE(SUM(cost),0)
		FROM budget_calls WHERE day=? AND status='settled' GROUP BY `+by+` ORDER BY SUM(cost) DESC, SUM(tokens) DESC, `+by, UTCDay(at))
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageShare
	for rows.Next() {
		var u UsageShare
		if err := rows.Scan(&u.Name, &u.Calls, &u.Tokens, &u.Cost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
