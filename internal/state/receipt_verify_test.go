package state

import (
	"context"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
)

// recordSigned writes one signed approval row and returns the fields used, so a
// test can reconstruct or tamper with them.
func recordSigned(t *testing.T, s *StateStore, secret []byte) approval.Fields {
	t.Helper()
	f := approval.Fields{
		Pipeline: "p", Step: "gate", DecidedAt: 1_700_000_000, Decision: "approve",
		OperatorID: 111, PayloadHash: hashOf("the exact draft"), QuorumN: 1, QuorumGot: 1,
	}
	nonce, err := approval.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	sig := approval.Sign(secret, f, nonce)
	if err := s.RecordApproval(f.Pipeline, f.Step, time.Unix(f.DecidedAt, 0), f.Decision,
		f.OperatorID, f.PayloadHash, f.QuorumN, f.QuorumGot, nonce, sig); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	return f
}

func TestVerifyApprovalsSignedRoundtrip(t *testing.T) {
	s := openTestStore(t)
	secret := []byte("audit-secret")
	recordSigned(t, s, secret)

	res, err := s.VerifyApprovals(secret, "p", 10)
	if err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if len(res) != 1 || res[0].Status != "ok" {
		t.Fatalf("want one ok row, got %+v", res)
	}
}

// The whole point: an edit made directly to the SQLite file — behind the
// append-only convention — is caught because it no longer matches the receipt.
func TestVerifyApprovalsDetectsTamper(t *testing.T) {
	s := openTestStore(t)
	secret := []byte("audit-secret")
	recordSigned(t, s, secret)

	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE action_approvals SET operator_id=999 WHERE pipeline='p'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := s.VerifyApprovals(secret, "p", 10)
	if err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if len(res) != 1 || res[0].Status != "tampered" {
		t.Fatalf("want tampered, got %+v", res)
	}
}

// A wrong secret must not silently pass — it reads as tampered, not ok.
func TestVerifyApprovalsWrongSecret(t *testing.T) {
	s := openTestStore(t)
	recordSigned(t, s, []byte("real-secret"))

	res, err := s.VerifyApprovals([]byte("wrong-secret"), "p", 10)
	if err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if len(res) != 1 || res[0].Status != "tampered" {
		t.Fatalf("want tampered under wrong secret, got %+v", res)
	}
}

func TestVerifyApprovalsUnsigned(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordApproval("p", "gate", time.Unix(1_700_000_000, 0), "approve", 111,
		hashOf("draft"), 1, 1, "", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	res, err := s.VerifyApprovals([]byte("secret"), "p", 10)
	if err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if len(res) != 1 || res[0].Status != "unsigned" {
		t.Fatalf("want unsigned, got %+v", res)
	}
}
