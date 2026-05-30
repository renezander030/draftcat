package state

import (
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
