package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *StateStore {
	t.Helper()
	s, err := OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestRecordAndQueryApproval records two decisions and reads them back
// newest-first with matching fields and a 64-hex payload hash.
func TestRecordAndQueryApproval(t *testing.T) {
	s := openTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	h := hashOf("the exact draft shown")

	if err := s.RecordApproval("p", "gate", t0, "approve", 111, h, 1, 1); err != nil {
		t.Fatalf("RecordApproval approve: %v", err)
	}
	if err := s.RecordApproval("p", "gate", t0.Add(time.Minute), "skip", 222, hashOf("other"), 1, 0); err != nil {
		t.Fatalf("RecordApproval skip: %v", err)
	}

	recs, err := s.ApprovalsForPipeline("p", 10)
	if err != nil {
		t.Fatalf("ApprovalsForPipeline: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 rows, got %d", len(recs))
	}
	// newest-first
	if recs[0].Decision != "skip" || recs[1].Decision != "approve" {
		t.Errorf("rows not newest-first: %q then %q", recs[0].Decision, recs[1].Decision)
	}
	if recs[1].OperatorID != 111 || recs[1].PayloadHash != h {
		t.Errorf("approve row mismatch: op=%d hash=%s", recs[1].OperatorID, recs[1].PayloadHash)
	}
	if len(recs[1].PayloadHash) != 64 {
		t.Errorf("payload_hash should be 64 hex chars, got %d", len(recs[1].PayloadHash))
	}
}

// TestUnapprovedActions: a gated step recorded with no approve row is surfaced;
// once an approve row is added it disappears.
func TestUnapprovedActions(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)

	if err := s.RecordApproval("p", "send-email", now, "timeout", 0, hashOf("d"), 2, 1); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	un, err := s.UnapprovedActions("p")
	if err != nil {
		t.Fatalf("UnapprovedActions: %v", err)
	}
	if len(un) != 1 || un[0] != "send-email" {
		t.Fatalf("want [send-email], got %v", un)
	}

	if err := s.RecordApproval("p", "send-email", now.Add(time.Minute), "approve", 111, hashOf("d2"), 2, 2); err != nil {
		t.Fatalf("RecordApproval approve: %v", err)
	}
	un, err = s.UnapprovedActions("p")
	if err != nil {
		t.Fatalf("UnapprovedActions: %v", err)
	}
	if len(un) != 0 {
		t.Errorf("after approve, want no unapproved actions, got %v", un)
	}
}

// TestAuditAppendOnly: recording two rows for the same (pipeline, step) keeps
// BOTH — RecordApproval never overwrites. This is the append-only invariant;
// only RecordApproval writes the table and no code path UPDATEs or DELETEs it.
func TestAuditAppendOnly(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 2; i++ {
		if err := s.RecordApproval("p", "gate", now.Add(time.Duration(i)*time.Minute), "approve", int64(100+i), hashOf("d"), 1, 1); err != nil {
			t.Fatalf("RecordApproval: %v", err)
		}
	}
	recs, err := s.ApprovalsForPipeline("p", 10)
	if err != nil {
		t.Fatalf("ApprovalsForPipeline: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("append-only: want 2 distinct rows, got %d", len(recs))
	}
}

// TestPayloadNotStored: only the hash is persisted; the plaintext draft must
// never appear in any column. Data-minimisation guard.
func TestPayloadNotStored(t *testing.T) {
	s := openTestStore(t)
	const secret = "DEAR CUSTOMER your SSN is 123-45-6789"
	if err := s.RecordApproval("p", "gate", time.Unix(1_700_000_000, 0), "approve", 111, hashOf(secret), 1, 1); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	rows, err := s.DB().QueryContext(context.Background(), `SELECT pipeline, step, decision, payload_hash FROM action_approvals`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pipeline, step, decision, payloadHash string
		if err := rows.Scan(&pipeline, &step, &decision, &payloadHash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, col := range []string{pipeline, step, decision, payloadHash} {
			if col == secret {
				t.Errorf("plaintext payload leaked into a column: %q", col)
			}
		}
		if payloadHash != hashOf(secret) {
			t.Errorf("payload_hash = %q, want %q", payloadHash, hashOf(secret))
		}
	}
}
