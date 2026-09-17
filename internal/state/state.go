package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
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
    error_text  TEXT,
    run_id      TEXT NOT NULL DEFAULT ''       -- minted at run start; joins to action_approvals
);
CREATE INDEX IF NOT EXISTS idx_runs_pipeline ON pipeline_runs(pipeline, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_runid ON pipeline_runs(run_id);
CREATE TABLE IF NOT EXISTS action_approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline     TEXT    NOT NULL,
    step         TEXT    NOT NULL,
    decided_at   INTEGER NOT NULL,           -- unix seconds
    decision     TEXT    NOT NULL,           -- approve|skip|adjust|timeout|quorum_fail
    operator_id  INTEGER NOT NULL,           -- TG user id; 0 for system/timeout
    payload_hash TEXT    NOT NULL,           -- sha256 hex of the exact draft shown
    quorum_n     INTEGER NOT NULL DEFAULT 1, -- approvals required
    quorum_got   INTEGER NOT NULL DEFAULT 1, -- approvals collected
    nonce        TEXT    NOT NULL DEFAULT '', -- per-row random; anti-replay
    signature    TEXT    NOT NULL DEFAULT '', -- HMAC receipt over the row's fields (empty = unsigned)
    run_id       TEXT    NOT NULL DEFAULT '', -- the pipeline run this decision released
    policy       TEXT    NOT NULL DEFAULT '', -- rule that released it when decision='policy_approve'
    receipt_version INTEGER NOT NULL DEFAULT 1,
    receipt_id   TEXT    NOT NULL DEFAULT '',
    action_id    TEXT    NOT NULL DEFAULT '',
    policy_hash  TEXT    NOT NULL DEFAULT '',
    binding_hash TEXT    NOT NULL DEFAULT '',
    expires_at   INTEGER NOT NULL DEFAULT 0,
    lifecycle    TEXT    NOT NULL DEFAULT 'decided'
);
CREATE INDEX IF NOT EXISTS idx_approvals_pipeline ON action_approvals(pipeline, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_approvals_runid ON action_approvals(run_id);
CREATE TABLE IF NOT EXISTS pending_approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline     TEXT    NOT NULL,
    step         TEXT    NOT NULL,
    payload_hash TEXT    NOT NULL,           -- sha256 hex of the draft shown (never the draft)
    quorum_n     INTEGER NOT NULL DEFAULT 1, -- approvals required
    opened_at    INTEGER NOT NULL,           -- unix seconds the gate was opened
    expires_at   INTEGER NOT NULL,           -- unix seconds the gate would time out
    status       TEXT    NOT NULL            -- pending|resolved|interrupted
);
CREATE INDEX IF NOT EXISTS idx_pending_status ON pending_approvals(status, opened_at);
CREATE TABLE IF NOT EXISTS tool_actions (
    action_id    TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    agent        TEXT NOT NULL DEFAULT '',
    run_id       TEXT NOT NULL DEFAULT '',
    args_hash    TEXT NOT NULL,
    policy_hash  TEXT NOT NULL,
    binding_hash TEXT NOT NULL,
    status       TEXT NOT NULL,
    decision     TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    decided_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    consumed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tool_actions_status ON tool_actions(status, updated_at);
CREATE TABLE IF NOT EXISTS webhook_admissions (
    id          TEXT PRIMARY KEY,
    pipeline    TEXT NOT NULL,
    body_hash   TEXT NOT NULL,
    status      TEXT NOT NULL,
    error_text  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_admissions_status ON webhook_admissions(status, created_at);
`
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		return err
	}
	// Migrate stores created before signed receipts existed: add the columns if
	// missing. SQLite has no "ADD COLUMN IF NOT EXISTS", so run the ALTER and
	// tolerate the duplicate-column error on stores that already have them.
	for _, alter := range []string{
		`ALTER TABLE action_approvals ADD COLUMN nonce TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN signature TEXT NOT NULL DEFAULT ''`,
		// run_id is a correlation column, deliberately outside the receipt
		// signature: adding a field to internal/approval.Fields would
		// invalidate the HMAC on every row signed before this release, so an
		// existing store would fail audit-verify after an upgrade. Rows written
		// from here on carry both a run_id and a receipt that still verifies.
		`ALTER TABLE action_approvals ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pipeline_runs ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN policy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN receipt_version INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE action_approvals ADD COLUMN receipt_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN action_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN policy_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN binding_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE action_approvals ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE action_approvals ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'decided'`,
	} {
		if _, err := db.ExecContext(context.Background(), alter); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
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

// ApprovalRecord is one row of the action_approvals audit table. Nonce and
// Signature are the tamper-evidence receipt: empty on rows written before signing
// was configured (or when no secret is set), otherwise an HMAC over the other
// fields under the operator's approval-signing secret.
// AllRecentRuns is RecentRuns across every pipeline, newest first. Used by
// `draftcat runs` when no pipeline is named.
func (s *StateStore) AllRecentRuns(n int) ([]RunRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT pipeline, started_at, ended_at, status, COALESCE(error_text,'')
		   FROM pipeline_runs ORDER BY started_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

type ApprovalRecord struct {
	ID          int64
	Version     int
	ReceiptID   string
	RunID       string
	ActionID    string
	Pipeline    string
	Step        string
	DecidedAt   time.Time
	Decision    string
	OperatorID  int64
	PayloadHash string
	QuorumN     int
	QuorumGot   int
	Nonce       string
	Signature   string
	Policy      string
	PolicyHash  string
	BindingHash string
	ExpiresAt   time.Time
	Lifecycle   string
}

// ApprovalEnvelope contains every immutable field written into a v2 receipt.
// Payloads remain represented only by hashes.
type ApprovalEnvelope struct {
	ReceiptID   string
	RunID       string
	ActionID    string
	Pipeline    string
	Step        string
	DecidedAt   time.Time
	Decision    string
	OperatorID  int64
	PayloadHash string
	QuorumN     int
	QuorumGot   int
	Nonce       string
	Signature   string
	Policy      string
	PolicyHash  string
	BindingHash string
	ExpiresAt   time.Time
	Lifecycle   string
}

// RecordApproval appends one approval-decision row. Called on every terminal
// decision (approve/skip/adjust/timeout/quorum_fail). Best-effort: a failure is
// surfaced to the caller but must not halt the engine. nonce and signature carry
// the receipt; pass "" for both to record an unsigned row (no secret configured).
func (s *StateStore) RecordApproval(pipeline, step string, decidedAt time.Time,
	decision string, operatorID int64, payloadHash string, quorumN, quorumGot int,
	nonce, signature string) error {
	return s.RecordApprovalForRun("", pipeline, step, decidedAt, decision, operatorID, payloadHash, quorumN, quorumGot, nonce, signature)
}

// RecordApprovalForRun is RecordApproval with the run identity attached, so the
// audit trail can answer which run a decision released rather than only which
// pipeline. runID may be "" for callers with no run context (the standalone
// approval path), which reproduces the previous behavior exactly.
func (s *StateStore) RecordApprovalForRun(runID, pipeline, step string, decidedAt time.Time,
	decision string, operatorID int64, payloadHash string, quorumN, quorumGot int,
	nonce, signature string) error {
	return s.RecordApprovalRow(runID, pipeline, step, decidedAt, decision, operatorID, payloadHash, quorumN, quorumGot, nonce, signature, "")
}

// RecordApprovalRow is the full write, including the policy rule that released
// a decision. policy is non-empty only for decision "policy_approve", so the
// audit trail always distinguishes what a human tapped from what a
// pre-declared rule released.
func (s *StateStore) RecordApprovalRow(runID, pipeline, step string, decidedAt time.Time,
	decision string, operatorID int64, payloadHash string, quorumN, quorumGot int,
	nonce, signature, policy string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO action_approvals (pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got, nonce, signature, run_id, policy)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pipeline, step, decidedAt.Unix(), decision, operatorID, payloadHash, quorumN, quorumGot, nonce, signature, runID, policy,
	)
	return err
}

// RecordApprovalV2 appends a versioned, action-bound receipt. Legacy writers
// continue through RecordApprovalRow and retain v1 verification semantics.
func (s *StateStore) RecordApprovalV2(e ApprovalEnvelope) error {
	lifecycle := e.Lifecycle
	if lifecycle == "" {
		lifecycle = "decided"
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO action_approvals
		 (pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got,
		  nonce, signature, run_id, policy, receipt_version, receipt_id, action_id, policy_hash,
		  binding_hash, expires_at, lifecycle)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 2, ?, ?, ?, ?, ?, ?)`,
		e.Pipeline, e.Step, e.DecidedAt.Unix(), e.Decision, e.OperatorID, e.PayloadHash,
		e.QuorumN, e.QuorumGot, e.Nonce, e.Signature, e.RunID, e.Policy, e.ReceiptID,
		e.ActionID, e.PolicyHash, e.BindingHash, e.ExpiresAt.Unix(), lifecycle,
	)
	return err
}

const approvalColumns = `id, receipt_version, receipt_id, run_id, action_id,
	 pipeline, step, decided_at, decision, operator_id, payload_hash, quorum_n, quorum_got,
	 nonce, signature, policy, policy_hash, binding_hash, expires_at, lifecycle`

func scanApproval(scan func(...interface{}) error) (ApprovalRecord, error) {
	var r ApprovalRecord
	var decided, expires int64
	err := scan(&r.ID, &r.Version, &r.ReceiptID, &r.RunID, &r.ActionID,
		&r.Pipeline, &r.Step, &decided, &r.Decision, &r.OperatorID, &r.PayloadHash,
		&r.QuorumN, &r.QuorumGot, &r.Nonce, &r.Signature, &r.Policy, &r.PolicyHash,
		&r.BindingHash, &expires, &r.Lifecycle)
	if err != nil {
		return r, err
	}
	r.DecidedAt = time.Unix(decided, 0)
	if expires > 0 {
		r.ExpiresAt = time.Unix(expires, 0)
	}
	return r, nil
}

// ApprovalsForRun returns every approval decision recorded against one run, in
// decision order. This is the join the audit trail previously could not make.
func (s *StateStore) ApprovalsForRun(runID string) ([]ApprovalRecord, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT `+approvalColumns+`
		 FROM action_approvals WHERE run_id=? ORDER BY decided_at ASC, id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovalRecord
	for rows.Next() {
		r, err := scanApproval(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApprovalsForPipeline returns the last n approval rows for a pipeline,
// newest-first.
func (s *StateStore) ApprovalsForPipeline(pipeline string, n int) ([]ApprovalRecord, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT `+approvalColumns+`
		 FROM action_approvals WHERE pipeline=? ORDER BY decided_at DESC, id DESC LIMIT ?`,
		pipeline, n,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovalRecord
	for rows.Next() {
		r, err := scanApproval(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllApprovals returns the newest receipt rows across every pipeline.
func (s *StateStore) AllApprovals(n int) ([]ApprovalRecord, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT `+approvalColumns+` FROM action_approvals ORDER BY decided_at DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovalRecord
	for rows.Next() {
		r, err := scanApproval(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApprovalByReceiptID looks up one receipt by its stable public id.
func (s *StateStore) ApprovalByReceiptID(id string) (ApprovalRecord, error) {
	row := s.db.QueryRowContext(context.Background(),
		`SELECT `+approvalColumns+` FROM action_approvals WHERE receipt_id=? OR (receipt_id='' AND CAST(id AS TEXT)=?) ORDER BY id DESC LIMIT 1`, id, id)
	return scanApproval(row.Scan)
}

// ApprovalVerification pairs an audit row with the result of checking its receipt.
type ApprovalVerification struct {
	Record ApprovalRecord
	Status string // "ok" | "tampered" | "unsigned"
}

// VerifyApprovalRecord checks one receipt and preserves the explicit unsigned
// state for rows written without a signing secret.
func VerifyApprovalRecord(secret []byte, r ApprovalRecord) string {
	if r.Signature == "" {
		return "unsigned"
	}
	ok := false
	if r.Version >= 2 {
		expires := int64(0)
		if !r.ExpiresAt.IsZero() {
			expires = r.ExpiresAt.Unix()
		}
		ok = approval.VerifyV2(secret, approval.FieldsV2{
			ReceiptID: r.ReceiptID, RunID: r.RunID, ActionID: r.ActionID,
			Pipeline: r.Pipeline, Step: r.Step, DecidedAt: r.DecidedAt.Unix(),
			Decision: r.Decision, OperatorID: r.OperatorID, PayloadHash: r.PayloadHash,
			Policy: r.Policy, PolicyHash: r.PolicyHash, BindingHash: r.BindingHash,
			ExpiresAt: expires, QuorumN: r.QuorumN, QuorumGot: r.QuorumGot,
		}, r.Nonce, r.Signature)
	} else {
		ok = approval.Verify(secret, approval.Fields{
			Pipeline: r.Pipeline, Step: r.Step, DecidedAt: r.DecidedAt.Unix(),
			Decision: r.Decision, OperatorID: r.OperatorID, PayloadHash: r.PayloadHash,
			QuorumN: r.QuorumN, QuorumGot: r.QuorumGot,
		}, r.Nonce, r.Signature)
	}
	if ok {
		return "ok"
	}
	return "tampered"
}

// VerifyApprovals re-checks the receipts on the last n approval rows for a
// pipeline under secret. "tampered" means the row's fields no longer match its
// signature — someone altered the audit trail after the decision was recorded.
// "unsigned" means the row was written with no secret configured. This is the
// read side of the tamper-evidence story: the append-only convention guards the
// engine's own writes; this catches edits made directly to the SQLite file.
func (s *StateStore) VerifyApprovals(secret []byte, pipeline string, n int) ([]ApprovalVerification, error) {
	recs, err := s.ApprovalsForPipeline(pipeline, n)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalVerification, 0, len(recs))
	for _, r := range recs {
		status := VerifyApprovalRecord(secret, r)
		out = append(out, ApprovalVerification{Record: r, Status: status})
	}
	return out, nil
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

// --- Pending approvals (crash/restart durability) ---
//
// An approval gate used to exist only inside an in-process poll loop: the
// engine sent the draft, blocked on a ticker, and wrote to action_approvals
// only once a decision arrived. If the process restarted while a gate was open
// — a redeploy, an OOM, a VPS reboot — the open gate left no trace at all. The
// operator saw a live-looking message with buttons, the run was gone, and
// UnapprovedActions could not see the hole because nothing was ever written.
//
// These three calls make the gate durable: a row is written BEFORE the draft is
// sent, resolved when a decision arrives, and reconciled to `interrupted` on
// the next boot if neither happened. The outcome of every gate is therefore
// always knowable, even the ones that were cut off mid-flight.

// PendingApproval is one approval gate that was open when the process stopped.
type PendingApproval struct {
	ID          int64
	Pipeline    string
	Step        string
	PayloadHash string
	QuorumN     int
	OpenedAt    time.Time
	ExpiresAt   time.Time
}

// BeginApproval records an approval gate as open and returns its row id. Called
// immediately BEFORE the draft goes out to the operator channel, so a crash in
// the window between sending and deciding is still visible afterwards. Like the
// audit table it stores only the sha256 of the draft, never the draft itself.
func (s *StateStore) BeginApproval(pipeline, step, payloadHash string, quorumN int, openedAt, expiresAt time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	res, err := s.db.ExecContext(context.Background(),
		`INSERT INTO pending_approvals (pipeline, step, payload_hash, quorum_n, opened_at, expires_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending')`,
		pipeline, step, payloadHash, quorumN, openedAt.Unix(), expiresAt.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResolveApproval closes an open gate once a terminal decision was reached.
// A zero id is a no-op so callers need not special-case a store-less run.
func (s *StateStore) ResolveApproval(id int64) error {
	if s == nil || s.db == nil || id == 0 {
		return nil
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE pending_approvals SET status='resolved' WHERE id=? AND status='pending'`, id)
	return err
}

// InterruptedApprovals returns every gate still marked pending — by definition
// gates that were open when the process last stopped, since a live run resolves
// its own row. Call once on boot, before the engine starts.
func (s *StateStore) InterruptedApprovals() ([]PendingApproval, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT id, pipeline, step, payload_hash, quorum_n, opened_at, expires_at
		   FROM pending_approvals WHERE status='pending' ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PendingApproval
	for rows.Next() {
		var p PendingApproval
		var opened, expires int64
		if err := rows.Scan(&p.ID, &p.Pipeline, &p.Step, &p.PayloadHash, &p.QuorumN, &opened, &expires); err != nil {
			return nil, err
		}
		p.OpenedAt = time.Unix(opened, 0)
		p.ExpiresAt = time.Unix(expires, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// OpenApprovals returns the gates currently waiting on a human, oldest first.
// Same rows as InterruptedApprovals — the difference is who is asking: the
// reconciler asks at boot, when "still pending" means "orphaned"; the
// operator's /pending command and `draftcat pending` ask while the engine is
// live, when it means "waiting on me". Either way the answer is the table.
func (s *StateStore) OpenApprovals() ([]PendingApproval, error) {
	return s.InterruptedApprovals()
}

// MarkInterrupted flips a pending gate to `interrupted`, the terminal state for
// "the process died before the operator decided". The caller is expected to
// also write an audit row so the compliance queries see it.
func (s *StateStore) MarkInterrupted(id int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE pending_approvals SET status='interrupted' WHERE id=? AND status='pending'`, id)
	return err
}

// ToolAction is the durable state machine behind an execution permit.
type ToolAction struct {
	ActionID    string
	Tool        string
	Agent       string
	RunID       string
	ArgsHash    string
	PolicyHash  string
	BindingHash string
	Status      string
	Decision    string
	Reason      string
	DecidedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
	ConsumedAt  time.Time
}

func scanToolAction(scan func(...interface{}) error) (ToolAction, error) {
	var a ToolAction
	var created, updated, expires, consumed int64
	err := scan(&a.ActionID, &a.Tool, &a.Agent, &a.RunID, &a.ArgsHash, &a.PolicyHash,
		&a.BindingHash, &a.Status, &a.Decision, &a.Reason, &a.DecidedBy,
		&created, &updated, &expires, &consumed)
	if err != nil {
		return a, err
	}
	a.CreatedAt, a.UpdatedAt, a.ExpiresAt = time.Unix(created, 0), time.Unix(updated, 0), time.Unix(expires, 0)
	if consumed > 0 {
		a.ConsumedAt = time.Unix(consumed, 0)
	}
	return a, nil
}

const toolActionColumns = `action_id, tool, agent, run_id, args_hash, policy_hash,
	 binding_hash, status, decision, reason, decided_by, created_at, updated_at, expires_at, consumed_at`

// ReserveToolAction atomically creates an action identity or returns the
// existing record for an idempotent retry. A mismatched binding is an error.
func (s *StateStore) ReserveToolAction(a ToolAction) (ToolAction, bool, error) {
	if s == nil || s.db == nil {
		return a, true, nil
	}
	res, err := s.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO tool_actions
		 (action_id, tool, agent, run_id, args_hash, policy_hash, binding_hash, status,
		  decision, reason, decided_by, created_at, updated_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', '', '', '', ?, ?, ?)`,
		a.ActionID, a.Tool, a.Agent, a.RunID, a.ArgsHash, a.PolicyHash, a.BindingHash,
		a.CreatedAt.Unix(), a.UpdatedAt.Unix(), a.ExpiresAt.Unix())
	if err != nil {
		return ToolAction{}, false, err
	}
	n, _ := res.RowsAffected()
	existing, err := s.ToolAction(a.ActionID)
	if err != nil {
		return ToolAction{}, false, err
	}
	if existing.BindingHash != a.BindingHash {
		return existing, false, fmt.Errorf("action_id %q is already bound to different action data", a.ActionID)
	}
	return existing, n == 1, nil
}

// ToolAction returns one durable permit state.
func (s *StateStore) ToolAction(id string) (ToolAction, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(context.Background(),
		`SELECT `+toolActionColumns+` FROM tool_actions WHERE action_id=?`, id)
	return scanToolAction(row.Scan)
}

// DecideToolAction transitions a pending action to allowed or denied.
func (s *StateStore) DecideToolAction(id, status, decision, reason, decidedBy string, at time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE tool_actions SET status=?, decision=?, reason=?, decided_by=?, updated_at=?
		 WHERE action_id=? AND status='pending'`, status, decision, reason, decidedBy, at.Unix(), id)
	return err
}

// ConsumeToolAction atomically spends one allowed permit. Only the first
// matching caller before expiry succeeds.
func (s *StateStore) ConsumeToolAction(id, bindingHash string, at time.Time) (ToolAction, bool, error) {
	if s == nil || s.db == nil {
		return ToolAction{}, false, nil
	}
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE tool_actions SET status='consumed', consumed_at=?, updated_at=?
		 WHERE action_id=? AND binding_hash=? AND status='allowed' AND expires_at>=?`,
		at.Unix(), at.Unix(), id, bindingHash, at.Unix())
	if err != nil {
		return ToolAction{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := s.db.ExecContext(context.Background(),
			`UPDATE tool_actions SET status='expired', decision='deny',
			 reason='permit expired before consumption', updated_at=?
			 WHERE action_id=? AND binding_hash=? AND status='allowed' AND expires_at<?`,
			at.Unix(), id, bindingHash, at.Unix()); err != nil {
			return ToolAction{}, false, err
		}
	}
	a, err := s.ToolAction(id)
	return a, n == 1, err
}

// ExpireToolActions closes pending work that cannot safely resume after a
// restart and allowed permits whose signed validity window has elapsed. A
// still-valid allowed permit remains consumable because its complete binding
// is durable.
func (s *StateStore) ExpireToolActions(at time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(context.Background(),
		`UPDATE tool_actions SET status='expired', decision='deny',
		 reason='process restarted before decision', updated_at=? WHERE status='pending'`, at.Unix()); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.ExecContext(context.Background(),
		`UPDATE tool_actions SET status='expired', decision='deny',
		 reason='permit expired before consumption', updated_at=?
		 WHERE status='allowed' AND expires_at<?`, at.Unix(), at.Unix()); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WebhookAdmission is the durable evidence that an HTTP trigger was accepted.
type WebhookAdmission struct {
	ID        string
	Pipeline  string
	BodyHash  string
	Status    string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BeginWebhookAdmission writes the accepted record before the handler returns 202.
func (s *StateStore) BeginWebhookAdmission(a WebhookAdmission) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("state store unavailable")
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO webhook_admissions (id, pipeline, body_hash, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'accepted', ?, ?)`, a.ID, a.Pipeline, a.BodyHash, a.CreatedAt.Unix(), a.UpdatedAt.Unix())
	return err
}

// FinishWebhookAdmission records the terminal pipeline outcome.
func (s *StateStore) FinishWebhookAdmission(id, status, errorText string, at time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE webhook_admissions SET status=?, error_text=?, updated_at=? WHERE id=?`,
		status, errorText, at.Unix(), id)
	return err
}

// WebhookAdmission returns the status exposed by the authenticated poll route.
func (s *StateStore) WebhookAdmission(id string) (WebhookAdmission, error) {
	if s == nil || s.db == nil {
		return WebhookAdmission{}, sql.ErrNoRows
	}
	var a WebhookAdmission
	var created, updated int64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT id, pipeline, body_hash, status, error_text, created_at, updated_at
		 FROM webhook_admissions WHERE id=?`, id).Scan(
		&a.ID, &a.Pipeline, &a.BodyHash, &a.Status, &a.Error, &created, &updated)
	if err != nil {
		return a, err
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return a, nil
}

// InterruptWebhookAdmissions makes accepted work visible after a restart.
func (s *StateStore) InterruptWebhookAdmissions(at time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE webhook_admissions SET status='interrupted', error_text='process restarted before completion', updated_at=?
		 WHERE status IN ('accepted','running')`, at.Unix())
	return err
}

// Ping checks whether the state store can serve durable decisions.
func (s *StateStore) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("state store unavailable")
	}
	return s.db.PingContext(ctx)
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
