package state

import (
	"context"
	"time"
)

// ApprovalsBetween returns the receipts decided in [from, to), oldest first,
// optionally for one pipeline.
func (s *StateStore) ApprovalsBetween(from, to time.Time, pipeline string) ([]ApprovalRecord, error) {
	if pipeline != "" {
		return s.queryApprovals(`WHERE decided_at >= ? AND decided_at < ? AND pipeline = ? ORDER BY decided_at, id`, from.Unix(), to.Unix(), pipeline)
	}
	return s.queryApprovals(`WHERE decided_at >= ? AND decided_at < ? ORDER BY decided_at, id`, from.Unix(), to.Unix())
}

// ToolOutcomeCounts counts reported tool executions completed in [from, to),
// per tool and status (succeeded, failed).
func (s *StateStore) ToolOutcomeCounts(from, to time.Time) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT t.tool, o.status, COUNT(*) FROM tool_execution_outcomes o
		 JOIN tool_actions t ON t.action_id = o.action_id
		 WHERE o.completed_at >= ? AND o.completed_at < ?
		 GROUP BY t.tool, o.status`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]int{}
	for rows.Next() {
		var tool, status string
		var n int
		if err := rows.Scan(&tool, &status, &n); err != nil {
			return nil, err
		}
		if out[tool] == nil {
			out[tool] = map[string]int{}
		}
		out[tool][status] = n
	}
	return out, rows.Err()
}
