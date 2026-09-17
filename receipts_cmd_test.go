package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestReceiptV2RoundTripAndView(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	secret := []byte("receipt-secret")
	decided := time.Unix(1_800_000_000, 0)
	e := newApprovalEnvelope(secret, "run-1", "pipeline", config.StepConfig{Name: "send", Risk: "high"},
		decided, decided.Add(time.Hour), "approve", 42, "sha256:payload", 1, 1, "human-approval")
	if err := st.RecordApprovalV2(e); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AllApprovals(10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	v := receiptView(rows[0], secret)
	if v.Version != 2 || v.ReceiptID == "" || v.ActionID == "" || v.BindingHash == "" || v.Verification != "ok" {
		t.Fatalf("receipt=%+v", v)
	}
	got, err := st.ApprovalByReceiptID(v.ReceiptID)
	if err != nil || got.ReceiptID != v.ReceiptID {
		t.Fatalf("show=%+v err=%v", got, err)
	}
}
