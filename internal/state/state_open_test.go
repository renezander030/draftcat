package state

import (
	"testing"
	"time"
)

// OpenApprovals is the live read of the same rows the reconciler consumes:
// pending until resolved, gone once a terminal decision closed the gate.
func TestOpenApprovals_ListsPendingUntilResolved(t *testing.T) {
	s := newTempStore(t)
	now := time.Now()
	a, _ := s.BeginApproval("invoices", "release", "h1", 1, now.Add(-time.Minute), now.Add(time.Hour))
	b, _ := s.BeginApproval("tool-gate", "send_email", "sha256:h2", 1, now, now.Add(time.Hour))

	open, err := s.OpenApprovals()
	if err != nil || len(open) != 2 {
		t.Fatalf("open = %d rows (err %v), want 2", len(open), err)
	}
	if open[0].ID != a || open[1].ID != b {
		t.Fatalf("open gates not oldest first: %+v", open)
	}

	if err := s.ResolveApproval(a); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	open, _ = s.OpenApprovals()
	if len(open) != 1 || open[0].Pipeline != "tool-gate" || open[0].Step != "send_email" {
		t.Fatalf("after resolving one, open = %+v, want only the tool-gate row", open)
	}
}

func TestOpenApprovals_NilStoreIsEmpty(t *testing.T) {
	var s *StateStore
	if rows, err := s.OpenApprovals(); err != nil || len(rows) != 0 {
		t.Fatalf("nil store returned %v, %v", rows, err)
	}
}
