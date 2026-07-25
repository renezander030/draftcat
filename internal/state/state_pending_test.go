package state

import (
	"testing"
	"time"
)

// The pending_approvals table exists so an approval gate that was open when the
// process stopped is still knowable afterwards. These tests pin the three
// transitions the reconciler depends on.

func TestPendingApproval_OpenGateIsReportedAfterRestart(t *testing.T) {
	s := newTempStore(t)

	now := time.Now()
	id, err := s.BeginApproval("invoices", "release", "abc123", 2, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("BeginApproval: %v", err)
	}
	if id == 0 {
		t.Fatal("BeginApproval returned id 0, want a real row id")
	}

	// A fresh process asks what was left open.
	open, err := s.InterruptedApprovals()
	if err != nil {
		t.Fatalf("InterruptedApprovals: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d open gate(s), want 1", len(open))
	}
	got := open[0]
	if got.Pipeline != "invoices" || got.Step != "release" {
		t.Errorf("gate = %s/%s, want invoices/release", got.Pipeline, got.Step)
	}
	if got.PayloadHash != "abc123" {
		t.Errorf("PayloadHash = %q, want abc123", got.PayloadHash)
	}
	if got.QuorumN != 2 {
		t.Errorf("QuorumN = %d, want 2", got.QuorumN)
	}
	if got.OpenedAt.Unix() != now.Unix() {
		t.Errorf("OpenedAt = %v, want %v", got.OpenedAt.Unix(), now.Unix())
	}
}

// A gate that reached a decision in-process must NOT be reported as
// interrupted on the next boot — otherwise every clean run would generate a
// false "your approval was lost" notice.
func TestPendingApproval_ResolvedGateIsNotInterrupted(t *testing.T) {
	s := newTempStore(t)

	now := time.Now()
	id, err := s.BeginApproval("leads", "gate", "hash", 1, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("BeginApproval: %v", err)
	}
	if err := s.ResolveApproval(id); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	open, err := s.InterruptedApprovals()
	if err != nil {
		t.Fatalf("InterruptedApprovals: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("got %d open gate(s) after resolve, want 0", len(open))
	}
}

// Reconciliation must be idempotent: booting twice cannot report the same
// interrupted gate twice, or a crash loop would spam the operator and inflate
// the audit log.
func TestPendingApproval_MarkInterruptedIsTerminal(t *testing.T) {
	s := newTempStore(t)

	now := time.Now()
	id, _ := s.BeginApproval("p", "gate", "h", 1, now, now.Add(time.Hour))

	if err := s.MarkInterrupted(id); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	open, err := s.InterruptedApprovals()
	if err != nil {
		t.Fatalf("InterruptedApprovals: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("second boot saw %d gate(s), want 0 — reconciliation is not idempotent", len(open))
	}

	// And a late resolve of an already-interrupted row must not resurrect it.
	if err := s.ResolveApproval(id); err != nil {
		t.Fatalf("ResolveApproval after interrupt: %v", err)
	}
	open, _ = s.InterruptedApprovals()
	if len(open) != 0 {
		t.Fatalf("row resurfaced after late resolve: %d", len(open))
	}
}

// Several gates can be open at once (concurrent pipelines); all must survive.
func TestPendingApproval_MultipleOpenGates(t *testing.T) {
	s := newTempStore(t)

	now := time.Now()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := s.BeginApproval(name, "gate", "h-"+name, 1, now, now.Add(time.Hour)); err != nil {
			t.Fatalf("BeginApproval(%s): %v", name, err)
		}
	}
	open, err := s.InterruptedApprovals()
	if err != nil {
		t.Fatalf("InterruptedApprovals: %v", err)
	}
	if len(open) != 3 {
		t.Fatalf("got %d open gate(s), want 3", len(open))
	}
}

// A nil store is the "no state configured" path. Every call must be a safe
// no-op so the engine still runs approvals without a database.
func TestPendingApproval_NilStoreIsSafe(t *testing.T) {
	var s *StateStore

	id, err := s.BeginApproval("p", "s", "h", 1, time.Now(), time.Now())
	if err != nil || id != 0 {
		t.Fatalf("BeginApproval on nil store = (%d, %v), want (0, nil)", id, err)
	}
	if err := s.ResolveApproval(0); err != nil {
		t.Fatalf("ResolveApproval on nil store: %v", err)
	}
	if err := s.MarkInterrupted(1); err != nil {
		t.Fatalf("MarkInterrupted on nil store: %v", err)
	}
	open, err := s.InterruptedApprovals()
	if err != nil || len(open) != 0 {
		t.Fatalf("InterruptedApprovals on nil store = (%v, %v), want (empty, nil)", open, err)
	}
}
