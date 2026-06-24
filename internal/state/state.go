package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// StateStore is a SQLite-backed persistence layer for cross-run pipeline
// state. Two responsibilities:
//   - Dedup: track item IDs (Gmail messages, GHL contacts/conversations) so
//     pipelines don't re-process the same item every scheduled tick.
//   - Audit: append-only log of pipeline runs (start, end, status, error)
//     for forensics.
//
// The store is opened once at startup and shared across all pipelines via the
// package-level `state` var.
type StateStore struct {
	db *sql.DB
}

func OpenStateStore(path string) (*StateStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state store %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		return nil, fmt.Errorf("state store pragmas: %w", err)
	}
	if err := initStateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &StateStore{db: db}, nil
}

func initStateSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS seen_items (
    pipeline TEXT NOT NULL,
    scope    TEXT NOT NULL,
    item_id  TEXT NOT NULL,
    seen_at  INTEGER NOT NULL,
    PRIMARY KEY (pipeline, scope, item_id)
);
CREATE TABLE IF NOT EXISTS pipeline_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline    TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER NOT NULL,
    status      TEXT NOT NULL,
    error_text  TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_pipeline ON pipeline_runs(pipeline, started_at DESC);
CREATE TABLE IF NOT EXISTS action_approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline     TEXT    NOT NULL,
    step         TEXT    NOT NULL,
    decided_at   INTEGER NOT NULL,           -- unix seconds
    decision     TEXT    NOT NULL,           -- approve|skip|adjust|timeout|quorum_fail
    operator_id  INTEGER NOT NULL,           -- TG user id; 0 for system/timeout
    payload_hash TEXT    NOT NULL,           -- sha256 hex of the exact draft shown
    quorum_n     INTEGER NOT NULL DEFAULT 1, -- approvals required
    quorum_got   INTEGER NOT NULL DEFAULT 1  -- approvals collected
);
CREATE INDEX IF NOT EXISTS idx_approvals_pipeline ON action_approvals(pipeline, decided_at DESC);
`
	_, err := db.Exec(schema)
	return err
}

// FilterUnseen returns the subset of ids not previously marked as seen for
// (pipeline, scope). Order is preserved.
func (s *StateStore) FilterUnseen(pipeline, scope string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	q := fmt.Sprintf(`SELECT item_id FROM seen_items WHERE pipeline=? AND scope=? AND item_id IN (%s)`, placeholders)
	args := make([]interface{}, 0, len(ids)+2)
	args = append(args, pipeline, scope)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		seen[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// MarkSeen records ids as seen for (pipeline, scope). Duplicate inserts are
// silently ignored.
func (s *StateStore) MarkSeen(pipeline, scope string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO seen_items (pipeline, scope, item_id, seen_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, id := range ids {
		if _, err := stmt.Exec(pipeline, scope, id, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// RecordRun appends a pipeline run record. Failures here are surfaced but
// must not halt the engine — observability is best-effort.
func (s *StateStore) RecordRun(pipeline string, started, ended time.Time, runErr error) error {
	status := "ok"
	var errText string
	if runErr != nil {
		status = "error"
		errText = runErr.Error()
	}
	_, err := s.db.Exec(
		`INSERT INTO pipeline_runs (pipeline, started_at, ended_at, status, error_text) VALUES (?, ?, ?, ?, ?)`,
		pipeline, started.Unix(), ended.Unix(), status, errText,
	)
	return err
}

// RecentRuns returns the last n runs for a pipeline, newest first. Used by
// the /status operator command.
type RunRecord struct {
	Pipeline  string
	StartedAt time.Time
	EndedAt   time.Time
	Status    string
	Error     string
}

func (s *StateStore) RecentRuns(pipeline string, n int) ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT pipeline, started_at, ended_at, status, COALESCE(error_text,'') FROM pipeline_runs WHERE pipeline=? ORDER BY started_at DESC LIMIT ?`,
		pipeline, n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		var st, en int64
		if err := rows.Scan(&r.Pipeline, &st, &en, &r.Status, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(st, 0)
		r.EndedAt = time.Unix(en, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Action approval audit (append-only) ---
//
// action_approvals is the durable "who approved which payload, when" log that
// backs the GDPR Art. 22 accountability story. It is append-only BY CONVENTION:
// only RecordApproval writes to it; no code path issues UPDATE or DELETE. It
// stores the sha256 of the draft (payload_hash), NEVER the draft itself — the
// audit log must not become a second copy of customer PII.

// ApprovalRecord is one row of the action_approvals audit table.
type ApprovalRecord struct {
	Pipeline    string
	Step        string
	DecidedAt   time.Time
	Decision    string
	OperatorID  int64
	PayloadHash string
	QuorumN     int
	QuorumGot   int
}

// RecordApproval appends one approval-decision row. Called on every terminal
// decision (approve/skip/adjust/timeout/quorum_fail). Best-effort: a failure is
// surfaced to the caller but must not halt the engine.
func (s *StateStore) RecordApproval(pipeline, step string, decidedAt time.Time,
	decision string, operatorID int64, payloadHash string, quorumN, quorumGot int) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO action_approvals (pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		pipeline, step, decidedAt.Unix(), decision, operatorID, payloadHash, quorumN, quorumGot,
	)
	return err
}

// ApprovalsForPipeline returns the last n approval rows for a pipeline,
// newest-first.
func (s *StateStore) ApprovalsForPipeline(pipeline string, n int) ([]ApprovalRecord, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got
		 FROM action_approvals WHERE pipeline=? ORDER BY decided_at DESC, id DESC LIMIT ?`,
		pipeline, n,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovalRecord
	for rows.Next() {
		var r ApprovalRecord
		var ts int64
		if err := rows.Scan(&r.Pipeline, &r.Step, &ts, &r.Decision, &r.OperatorID, &r.PayloadHash, &r.QuorumN, &r.QuorumGot); err != nil {
			return nil, err
		}
		r.DecidedAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnapprovedActions is a compliance query: it returns the distinct gated steps
// for a pipeline that were recorded in the audit log but never received an
// `approve` decision (only skip/timeout/quorum_fail). A non-empty result means a
// gated action lacks a matching approval — exactly the Art. 5(2)/30
// accountability check.
//
// Implementation note: draftcat has no separate "actions that ran" table — the
// audit log IS the record of gated decisions — so an unapproved action is a
// step present in action_approvals with no approve row. This is the defensible
// reading of the spec given the schema.
func (s *StateStore) UnapprovedActions(pipeline string) ([]string, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT DISTINCT step FROM action_approvals
		 WHERE pipeline=? AND step NOT IN (
		     SELECT step FROM action_approvals WHERE pipeline=? AND decision='approve'
		 ) ORDER BY step`,
		pipeline, pipeline,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, rows.Err()
}

func (s *StateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB so plugins (voice, etc.) can create their
// own tables in the same SQLite file and share the same WAL journal.
func (s *StateStore) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}
