package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// GateFreeze is the operator's stop on every gated tool call. By and At say
// who set the current state (freeze or lift) and when.
type GateFreeze struct {
	Frozen bool
	Reason string
	By     string
	At     time.Time
}

// SetGateFreeze freezes the tool gate. changed is false when it was already
// frozen; the first freeze keeps its reason, who and when.
func (s *StateStore) SetGateFreeze(reason, by string, at time.Time) (changed bool, err error) {
	if s == nil || s.db == nil {
		return false, ErrNoStore
	}
	res, err := s.db.ExecContext(context.Background(),
		`INSERT INTO gate_freeze (id, frozen, reason, by, at) VALUES (1, 1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET frozen=1, reason=excluded.reason, by=excluded.by, at=excluded.at WHERE gate_freeze.frozen=0`,
		reason, by, at.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearGateFreeze lifts the freeze. changed is false when it was not frozen.
func (s *StateStore) ClearGateFreeze(by string, at time.Time) (changed bool, err error) {
	if s == nil || s.db == nil {
		return false, ErrNoStore
	}
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE gate_freeze SET frozen=0, by=?, at=? WHERE id=1 AND frozen=1`, by, at.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GateFreeze reads the freeze. A store that never had one, or one the engine
// has not yet migrated, reports not frozen.
func (s *StateStore) GateFreeze() (GateFreeze, error) {
	if s == nil || s.db == nil {
		return GateFreeze{}, ErrNoStore
	}
	var f GateFreeze
	var frozen, at int64
	err := s.db.QueryRowContext(context.Background(), `SELECT frozen, reason, by, at FROM gate_freeze WHERE id=1`).Scan(&frozen, &f.Reason, &f.By, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return GateFreeze{}, nil
	case err != nil && isNoSuchTable(err):
		return GateFreeze{}, nil
	case err != nil:
		return GateFreeze{}, err
	}
	f.Frozen, f.At = frozen != 0, time.Unix(at, 0)
	return f, nil
}

// AgentFreeze is the stop on one agent's gated tool calls, set by
// tool_gate.trip. By and At say who set it and when.
type AgentFreeze struct {
	Agent  string
	Reason string
	By     string
	At     time.Time
}

// SetAgentFreeze freezes one agent. changed is false when it was already
// frozen; the first freeze keeps its reason, who and when.
func (s *StateStore) SetAgentFreeze(agent, reason, by string, at time.Time) (changed bool, err error) {
	if s == nil || s.db == nil {
		return false, ErrNoStore
	}
	res, err := s.db.ExecContext(context.Background(),
		`INSERT INTO gate_agent_freeze (agent, frozen, reason, by, at) VALUES (?, 1, ?, ?, ?)
		 ON CONFLICT(agent) DO UPDATE SET frozen=1, reason=excluded.reason, by=excluded.by, at=excluded.at WHERE gate_agent_freeze.frozen=0`,
		agent, reason, by, at.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearAgentFreeze lifts one agent's freeze. changed is false when the agent
// was not frozen.
func (s *StateStore) ClearAgentFreeze(agent, by string, at time.Time) (changed bool, err error) {
	if s == nil || s.db == nil {
		return false, ErrNoStore
	}
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE gate_agent_freeze SET frozen=0, by=?, at=? WHERE agent=? AND frozen=1`, by, at.Unix(), agent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AgentFreeze reads one agent's freeze; ok is false when it is not frozen. A
// store the engine has not yet migrated has no frozen agents.
func (s *StateStore) AgentFreeze(agent string) (f AgentFreeze, ok bool, err error) {
	if s == nil || s.db == nil {
		return AgentFreeze{}, false, ErrNoStore
	}
	var at int64
	err = s.db.QueryRowContext(context.Background(),
		`SELECT agent, reason, by, at FROM gate_agent_freeze WHERE agent=? AND frozen=1`, agent).Scan(&f.Agent, &f.Reason, &f.By, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows), err != nil && isNoSuchTable(err):
		return AgentFreeze{}, false, nil
	case err != nil:
		return AgentFreeze{}, false, err
	}
	f.At = time.Unix(at, 0)
	return f, true, nil
}

// AgentFreezes lists the frozen agents, the longest frozen first.
func (s *StateStore) AgentFreezes() ([]AgentFreeze, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT agent, reason, by, at FROM gate_agent_freeze WHERE frozen=1 ORDER BY at, agent`)
	if err != nil {
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentFreeze
	for rows.Next() {
		var f AgentFreeze
		var at int64
		if err := rows.Scan(&f.Agent, &f.Reason, &f.By, &at); err != nil {
			return nil, err
		}
		f.At = time.Unix(at, 0)
		out = append(out, f)
	}
	return out, rows.Err()
}

func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
