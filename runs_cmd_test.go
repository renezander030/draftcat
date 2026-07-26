package main

import (
	"errors"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

// approvalsDuring joins approval decisions onto the run they happened in by
// timestamp window. There is no run_id column, so this join is the whole
// correctness question for `draftcat runs`.

func TestApprovalsDuring_AttachesOnlyDecisionsInsideTheWindow(t *testing.T) {
	st := newTempStateStore(t)

	start := time.Now().Add(-10 * time.Minute)
	end := start.Add(5 * time.Minute)

	// Inside the window.
	if err := st.RecordApproval("p", "gate", start.Add(time.Minute), "approve", 111, "h1", 1, 1, "", ""); err != nil {
		t.Fatalf("RecordApproval inside: %v", err)
	}
	// Before the run started, and after it ended — both must be excluded.
	if err := st.RecordApproval("p", "old", start.Add(-time.Hour), "approve", 111, "h0", 1, 1, "", ""); err != nil {
		t.Fatalf("RecordApproval before: %v", err)
	}
	if err := st.RecordApproval("p", "later", end.Add(time.Hour), "skip", 222, "h2", 1, 0, "", ""); err != nil {
		t.Fatalf("RecordApproval after: %v", err)
	}

	got := approvalsDuring(st, statestore.RunRecord{
		Pipeline: "p", StartedAt: start, EndedAt: end, Status: "ok",
	})
	if len(got) != 1 {
		t.Fatalf("got %d approval(s) in window, want 1: %+v", len(got), got)
	}
	if got[0].Step != "gate" {
		t.Errorf("step = %q, want gate", got[0].Step)
	}
	if got[0].Decision != "approve" || got[0].OperatorID != 111 {
		t.Errorf("decision/operator = %s/%d, want approve/111", got[0].Decision, got[0].OperatorID)
	}
}

// A run of a different pipeline must never pick up this one's approvals.
func TestApprovalsDuring_ScopedToPipeline(t *testing.T) {
	st := newTempStateStore(t)
	start := time.Now().Add(-time.Minute)
	end := time.Now()

	if err := st.RecordApproval("other", "gate", start.Add(time.Second), "approve", 1, "h", 1, 1, "", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	got := approvalsDuring(st, statestore.RunRecord{Pipeline: "p", StartedAt: start, EndedAt: end})
	if len(got) != 0 {
		t.Fatalf("got %d approval(s) from another pipeline, want 0", len(got))
	}
}

// The signed flag drives a "[signed]" marker in the human output, so it must
// reflect whether a receipt was actually stored.
func TestApprovalsDuring_ReportsSignedReceipts(t *testing.T) {
	st := newTempStateStore(t)
	start := time.Now().Add(-time.Minute)
	end := time.Now()

	if err := st.RecordApproval("p", "signed-step", start.Add(time.Second), "approve", 1, "h", 1, 1, "nonce", "sig"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if err := st.RecordApproval("p", "plain-step", start.Add(2*time.Second), "approve", 1, "h", 1, 1, "", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	got := approvalsDuring(st, statestore.RunRecord{Pipeline: "p", StartedAt: start, EndedAt: end})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	signed := map[string]bool{}
	for _, a := range got {
		signed[a.Step] = a.Signed
	}
	if !signed["signed-step"] {
		t.Error("a row with a signature must report signed=true")
	}
	if signed["plain-step"] {
		t.Error("a row with no signature must report signed=false")
	}
}

// Ordering matters for reading a gate's history back: oldest decision first.
func TestApprovalsDuring_SortedOldestFirst(t *testing.T) {
	st := newTempStateStore(t)
	start := time.Now().Add(-time.Hour)
	end := time.Now()

	_ = st.RecordApproval("p", "third", start.Add(30*time.Minute), "approve", 1, "h", 1, 1, "", "")
	_ = st.RecordApproval("p", "first", start.Add(time.Minute), "adjust", 1, "h", 1, 0, "", "")
	_ = st.RecordApproval("p", "second", start.Add(10*time.Minute), "adjust", 1, "h", 1, 0, "", "")

	got := approvalsDuring(st, statestore.RunRecord{Pipeline: "p", StartedAt: start, EndedAt: end})
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if got[i].Step != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Step, w)
		}
	}
}

func TestAllRecentRuns_NewestFirstAcrossPipelines(t *testing.T) {
	st := newTempStateStore(t)
	base := time.Now().Add(-time.Hour)

	if err := st.RecordRun("alpha", base, base.Add(time.Minute), nil); err != nil {
		t.Fatalf("RecordRun alpha: %v", err)
	}
	if err := st.RecordRun("beta", base.Add(10*time.Minute), base.Add(11*time.Minute), errors.New("boom")); err != nil {
		t.Fatalf("RecordRun beta: %v", err)
	}

	runs, err := st.AllRecentRuns(10)
	if err != nil {
		t.Fatalf("AllRecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d run(s), want 2", len(runs))
	}
	if runs[0].Pipeline != "beta" {
		t.Errorf("newest run = %q, want beta", runs[0].Pipeline)
	}
	if runs[0].Status == "ok" {
		t.Errorf("a run recorded with an error should not be status ok, got %q", runs[0].Status)
	}
}

func TestAllRecentRuns_RespectsLimit(t *testing.T) {
	st := newTempStateStore(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		_ = st.RecordRun("p", base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute+time.Second), nil)
	}
	runs, err := st.AllRecentRuns(3)
	if err != nil {
		t.Fatalf("AllRecentRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d, want 3 (limit ignored)", len(runs))
	}
}

func TestAllRecentRuns_NilStoreIsSafe(t *testing.T) {
	var s *statestore.StateStore
	runs, err := s.AllRecentRuns(10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("nil store = (%v, %v), want (empty, nil)", runs, err)
	}
}
