package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/zkreceipt"
)

func TestLatestHumanApprovalDoesNotTreatPolicyAsHuman(t *testing.T) {
	records := []statestore.ApprovalRecord{
		{Decision: "policy_approve", DecidedAt: time.Now()},
		{Decision: "approve", DecidedAt: time.Now().Add(-time.Minute)},
	}
	record, ok := latestHumanApproval(records)
	if !ok || record.Decision != "approve" {
		t.Fatalf("wanted direct human approval, got %+v, %v", record, ok)
	}
}

func TestZKReceiptCommandEndToEnd(t *testing.T) {
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.db")
	proofPath := filepath.Join(directory, "approval.proof.json")
	secret := []byte("test approval secret is long enough")
	t.Setenv("DRAFTCAT_STATE_PATH", statePath)
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", string(secret))

	store, err := statestore.OpenStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Unix(1788580800, 0)
	fields := approval.Fields{
		Pipeline: "customer-refund", Step: "send-refund", DecidedAt: decidedAt.Unix(),
		Decision: "approve", OperatorID: 42, PayloadHash: "payload-sha256", QuorumN: 2, QuorumGot: 2,
	}
	nonce := "61f6ba5968994b80a1fa9e360fb173f1"
	if err := store.RecordApproval(fields.Pipeline, fields.Step, decidedAt, fields.Decision, fields.OperatorID,
		fields.PayloadHash, fields.QuorumN, fields.QuorumGot, nonce, approval.Sign(secret, fields, nonce)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if code := runZKReceiptProve([]string{"--out", proofPath, fields.Pipeline}); code != 0 {
		t.Fatalf("prove command exited %d", code)
	}
	data, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := zkreceipt.UnmarshalBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	expectedKey, err := zkreceipt.KeyCommitment(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := zkreceipt.Verify(bundle, expectedKey); err != nil {
		t.Fatalf("command produced invalid proof: %v", err)
	}
}
