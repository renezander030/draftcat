package state

import (
	"context"
	"fmt"
	"time"
)

// ShadowDiff is one live tool call that the shadow candidate policy decided
// differently from the live policy. The arguments are kept as their hash
// only, and rules and reasons in their value-free form.
type ShadowDiff struct {
	At             time.Time
	Agent          string
	Tool           string
	ArgsHash       string
	LiveDecision   string
	LiveRule       string
	LiveReason     string
	ShadowDecision string
	ShadowRule     string
	ShadowReason   string
	CandidateHash  string
}

// ShadowGroup counts the differences of one tool and decision pair.
type ShadowGroup struct {
	Tool           string
	LiveDecision   string
	ShadowDecision string
	Count          int
	Latest         time.Time
}

// RecordShadowDiff stores one difference and drops the rows older than
// pruneBefore, so the table holds a bounded window.
func (s *StateStore) RecordShadowDiff(d ShadowDiff, pruneBefore time.Time) error {
	if s == nil || s.db == nil {
		return ErrNoStore
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(), `DELETE FROM gate_shadow_diffs WHERE at < ?`, pruneBefore.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO gate_shadow_diffs (at, agent, tool, args_hash, live_decision, live_rule, live_reason,
		 shadow_decision, shadow_rule, shadow_reason, candidate_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.At.Unix(), d.Agent, d.Tool, d.ArgsHash, d.LiveDecision, d.LiveRule, d.LiveReason,
		d.ShadowDecision, d.ShadowRule, d.ShadowReason, d.CandidateHash); err != nil {
		return err
	}
	return tx.Commit()
}

// ShadowDiffs returns the latest differences at or after since, newest
// first, at most limit of them. A store the engine has not yet migrated has
// none.
func (s *StateStore) ShadowDiffs(since time.Time, limit int) ([]ShadowDiff, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT at, agent, tool, args_hash, live_decision, live_rule, live_reason,
		 shadow_decision, shadow_rule, shadow_reason, candidate_hash
		 FROM gate_shadow_diffs WHERE at >= ? ORDER BY at DESC, id DESC LIMIT ?`, since.Unix(), limit)
	if err != nil {
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ShadowDiff
	for rows.Next() {
		var d ShadowDiff
		var at int64
		if err := rows.Scan(&at, &d.Agent, &d.Tool, &d.ArgsHash, &d.LiveDecision, &d.LiveRule, &d.LiveReason,
			&d.ShadowDecision, &d.ShadowRule, &d.ShadowReason, &d.CandidateHash); err != nil {
			return nil, err
		}
		d.At = time.Unix(at, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ShadowDiffGroups counts the differences at or after since by tool and
// decision pair, the largest group first.
func (s *StateStore) ShadowDiffGroups(since time.Time) ([]ShadowGroup, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT tool, live_decision, shadow_decision, COUNT(*), MAX(at) FROM gate_shadow_diffs
		 WHERE at >= ? GROUP BY tool, live_decision, shadow_decision
		 ORDER BY COUNT(*) DESC, tool, live_decision, shadow_decision`, since.Unix())
	if err != nil {
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ShadowGroup
	for rows.Next() {
		var g ShadowGroup
		var latest int64
		if err := rows.Scan(&g.Tool, &g.LiveDecision, &g.ShadowDecision, &g.Count, &latest); err != nil {
			return nil, err
		}
		g.Latest = time.Unix(latest, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// ToolCallsAllowedOn counts a tool's actions allowed on a UTC day (created
// that day, consumed or not), leaving out exceptAction. It is the call count
// for a max_calls_per_day the live rules do not keep a count for.
func (s *StateStore) ToolCallsAllowedOn(tool, day, exceptAction string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, ErrNoStore
	}
	start, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0, fmt.Errorf("day %q: %w", day, err)
	}
	var n int64
	err = s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM tool_actions WHERE tool = ? AND decision = 'allow'
		 AND created_at >= ? AND created_at < ? AND action_id <> ?`,
		tool, start.Unix(), start.AddDate(0, 0, 1).Unix(), exceptAction).Scan(&n)
	return n, err
}
