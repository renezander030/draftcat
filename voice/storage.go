//go:build voice

package voice

import (
	"context"
	"database/sql"
	"errors"
)

const migrationSQL = `
CREATE TABLE IF NOT EXISTS voice_sessions (
  id TEXT PRIMARY KEY,
  caller_phone TEXT,
  workflow TEXT,
  started_at INTEGER NOT NULL,
  pre_call_context_json TEXT,
  ended_at INTEGER,
  status TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS voice_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  kind TEXT NOT NULL,
  payload_json TEXT
);

CREATE TABLE IF NOT EXISTS voice_outcomes (
  session_id TEXT PRIMARY KEY,
  outcome_json TEXT NOT NULL,
  transcript_json TEXT,
  recording_url TEXT,
  cost_cents INTEGER,
  metrics_json TEXT
);

CREATE TABLE IF NOT EXISTS voice_handoffs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  requested_at INTEGER NOT NULL,
  reason TEXT,
  target_resolved TEXT,
  resolved_at INTEGER
);

CREATE TABLE IF NOT EXISTS voice_learnings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  category TEXT,
  description TEXT NOT NULL,
  severity TEXT,
  proposed_change_json TEXT,
  review_status TEXT NOT NULL DEFAULT 'new'
);

CREATE INDEX IF NOT EXISTS idx_sessions_status ON voice_sessions (status);
CREATE INDEX IF NOT EXISTS idx_handoffs_unresolved ON voice_handoffs (resolved_at);
CREATE INDEX IF NOT EXISTS idx_learnings_status ON voice_learnings (review_status);
`

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(migrationSQL); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

type SessionStart struct {
	ID                 string
	CallerPhone        string
	Workflow           string
	StartedAt          int64
	PreCallContextJSON string
}

func (s *Store) InsertSessionStart(ss SessionStart) error {
	_, err := s.db.Exec(`
		INSERT INTO voice_sessions (id, caller_phone, workflow, started_at, pre_call_context_json, status)
		VALUES (?, ?, ?, ?, ?, 'active')
		ON CONFLICT(id) DO UPDATE SET
			caller_phone = excluded.caller_phone,
			workflow = excluded.workflow,
			started_at = excluded.started_at,
			pre_call_context_json = excluded.pre_call_context_json
	`, ss.ID, ss.CallerPhone, ss.Workflow, ss.StartedAt, ss.PreCallContextJSON)
	return err
}

// SessionStartedAt returns when a session started, in unix milliseconds, and
// whether its session_start was recorded.
func (s *Store) SessionStartedAt(ctx context.Context, id string) (int64, bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT started_at FROM voice_sessions WHERE id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return at, err == nil, err
}

type Event struct {
	SessionID   string
	TS          int64
	Kind        string
	PayloadJSON string
}

func (s *Store) InsertEvent(e Event) error {
	_, err := s.db.Exec(`
		INSERT INTO voice_events (session_id, ts, kind, payload_json)
		VALUES (?, ?, ?, ?)
	`, e.SessionID, e.TS, e.Kind, e.PayloadJSON)
	return err
}

type Outcome struct {
	SessionID      string
	OutcomeJSON    string
	TranscriptJSON string
	RecordingURL   string
	CostCents      int64
	MetricsJSON    string
	EndedAt        int64
}

func (s *Store) InsertOutcome(o Outcome) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO voice_outcomes (session_id, outcome_json, transcript_json, recording_url, cost_cents, metrics_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			outcome_json = excluded.outcome_json,
			transcript_json = excluded.transcript_json,
			recording_url = excluded.recording_url,
			cost_cents = excluded.cost_cents,
			metrics_json = excluded.metrics_json
	`, o.SessionID, o.OutcomeJSON, o.TranscriptJSON, o.RecordingURL, o.CostCents, o.MetricsJSON); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		UPDATE voice_sessions SET ended_at = ?, status = 'completed' WHERE id = ?
	`, o.EndedAt, o.SessionID); err != nil {
		return err
	}

	return tx.Commit()
}

type Handoff struct {
	SessionID   string
	RequestedAt int64
	Reason      string
}

func (s *Store) InsertHandoff(h Handoff) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO voice_handoffs (session_id, requested_at, reason)
		VALUES (?, ?, ?)
	`, h.SessionID, h.RequestedAt, h.Reason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type Learning struct {
	SessionID          string
	TS                 int64
	Category           string
	Description        string
	Severity           string
	ProposedChangeJSON string
}

func (s *Store) InsertLearning(l Learning) error {
	_, err := s.db.Exec(`
		INSERT INTO voice_learnings (session_id, ts, category, description, severity, proposed_change_json, review_status)
		VALUES (?, ?, ?, ?, ?, ?, 'new')
	`, l.SessionID, l.TS, l.Category, l.Description, l.Severity, l.ProposedChangeJSON)
	return err
}
