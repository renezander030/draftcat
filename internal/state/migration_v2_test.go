package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestV2SchemaMigratesExistingApprovalRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `CREATE TABLE action_approvals (
		id INTEGER PRIMARY KEY AUTOINCREMENT, pipeline TEXT NOT NULL, step TEXT NOT NULL,
		decided_at INTEGER NOT NULL, decision TEXT NOT NULL, operator_id INTEGER NOT NULL,
		payload_hash TEXT NOT NULL, quorum_n INTEGER NOT NULL DEFAULT 1,
		quorum_got INTEGER NOT NULL DEFAULT 1, nonce TEXT NOT NULL DEFAULT '',
		signature TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', policy TEXT NOT NULL DEFAULT '');
		INSERT INTO action_approvals (pipeline, step, decided_at, decision, operator_id, payload_hash)
		VALUES ('p', 'gate', 1800000000, 'approve', 7, 'sha256:old')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.AllApprovals(10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Version != 1 || rows[0].Lifecycle != "decided" || rows[0].DecidedAt != time.Unix(1_800_000_000, 0) {
		t.Fatalf("legacy row changed during migration: %+v", rows[0])
	}
	if _, _, err := st.ReserveToolAction(ToolAction{ActionID: "a", Tool: "t", ArgsHash: "h", PolicyHash: "p", BindingHash: "b", CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("new action ledger unavailable after migration: %v", err)
	}
}
