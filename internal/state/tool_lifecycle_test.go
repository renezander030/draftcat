package state

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func lifecycleAction(t *testing.T, s *StateStore, id string, now time.Time) ToolAction {
	t.Helper()
	a := ToolAction{ActionID: id, Tool: "send", ArgsHash: "args", PolicyHash: "policy", BindingHash: "binding", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, _, err := s.ReserveToolAction(a); err != nil {
		t.Fatal(err)
	}
	return a
}
func lifecycleReceipt(a ToolAction, at time.Time, decision string) ApprovalEnvelope {
	return ApprovalEnvelope{ReceiptID: "receipt-" + a.ActionID + "-" + decision, ActionID: a.ActionID, Pipeline: "tool-gate", Step: a.Tool, PayloadHash: a.ArgsHash, PolicyHash: a.PolicyHash, BindingHash: a.BindingHash, ExpiresAt: a.ExpiresAt, DecidedAt: at, Decision: decision, Lifecycle: decision}
}

func TestToolLifecycleRevokeConsumeRace(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 30; i++ {
		a := lifecycleAction(t, s, fmt.Sprintf("race-%d", i), now)
		if err := s.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", now); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var consumed, revoked bool
		var consumeErr, revokeErr error
		go func() {
			defer wg.Done()
			<-start
			_, consumed, consumeErr = s.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, now, lifecycleReceipt(a, now, "consume"))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revoked, revokeErr = s.CancelToolAction(a.ActionID, a.BindingHash, now)
		}()
		close(start)
		wg.Wait()
		if consumeErr != nil || revokeErr != nil || consumed == revoked {
			t.Fatalf("consume=%v revoke=%v errors=%v/%v", consumed, revoked, consumeErr, revokeErr)
		}
		got, err := s.ToolAction(a.ActionID)
		if err != nil {
			t.Fatal(err)
		}
		if revoked && got.Status != "revoked" || consumed && got.Status != "consumed" {
			t.Fatalf("race state %+v", got)
		}
		if revoked {
			if _, ok, err := s.CancelToolAction(a.ActionID, a.BindingHash, now); err != nil || !ok {
				t.Fatalf("revoke retry=%v %v", ok, err)
			}
			if got, err := s.DecideToolActionWithReceipt(a.ActionID, "allowed", "allow", "late approval", "operator", now, lifecycleReceipt(a, now, "approve")); err != nil || got.Status != "revoked" {
				t.Fatalf("late approval resurrected: %+v %v", got, err)
			}
		}
	}
}

func TestToolLifecycleAuditFailureRollsBackDecisionAndConsumption(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	a := lifecycleAction(t, s, "audit", now)
	if _, err := s.db.ExecContext(context.Background(), `CREATE TRIGGER reject_tool_receipt BEFORE INSERT ON action_approvals BEGIN SELECT RAISE(FAIL,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideToolActionWithReceipt(a.ActionID, "allowed", "allow", "approve", "operator", now, lifecycleReceipt(a, now, "approve")); err == nil {
		t.Fatal("failed audit permitted decision")
	}
	if got, _ := s.ToolAction(a.ActionID); got.Status != "pending" {
		t.Fatalf("decision escaped rollback: %+v", got)
	}
	if err := s.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, now, lifecycleReceipt(a, now, "consume")); err == nil || ok {
		t.Fatal("failed audit issued execution permit")
	}
	if got, _ := s.ToolAction(a.ActionID); got.Status != "allowed" {
		t.Fatalf("consumption escaped rollback: %+v", got)
	}
}

func TestToolLifecycleOutcomesImmutableAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	a := lifecycleAction(t, s, "outcome", now)
	o := ToolOutcome{ActionID: a.ActionID, BindingHash: a.BindingHash, Status: "succeeded", ResultHash: "sha256:result", CompletedAt: now}
	if _, ok, err := s.CompleteToolAction(o); err != nil || ok {
		t.Fatalf("unconsumed completion=%v %v", ok, err)
	}
	if err := s.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, now, lifecycleReceipt(a, now, "consume")); err != nil || !ok {
		t.Fatalf("consume=%v %v", ok, err)
	}
	if _, ok, err := s.CompleteToolAction(o); err != nil || !ok {
		t.Fatalf("complete=%v %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.ToolOutcome(a.ActionID); err != nil || got.Status != "succeeded" || got.ResultHash != o.ResultHash {
		t.Fatalf("restored outcome=%+v %v", got, err)
	}
	o.CompletedAt = now.Add(time.Minute)
	if got, ok, err := s.CompleteToolAction(o); err != nil || !ok || !got.CompletedAt.Equal(now) {
		t.Fatalf("retry mutated outcome=%+v %v %v", got, ok, err)
	}
	o.Status = "failed"
	if _, ok, err := s.CompleteToolAction(o); err != nil || ok {
		t.Fatalf("conflicting result accepted=%v %v", ok, err)
	}
	if _, ok, err := s.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, now, lifecycleReceipt(a, now, "consume-again")); err != nil || ok {
		t.Fatal("outcome reopened execution")
	}
}

func TestToolLifecycleExactExpiryAndPolicyNormalization(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	for _, kind := range []string{"expiry", "policy"} {
		a := lifecycleAction(t, s, kind, now)
		if err := s.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", now); err != nil {
			t.Fatal(err)
		}
		at, policy := a.ExpiresAt, a.PolicyHash
		if kind == "policy" {
			at, policy = now, "new-policy"
		}
		got, err := s.NormalizeToolAction(a.ActionID, policy, at)
		if err != nil || got.Status != "expired" || got.Decision != "deny" {
			t.Fatalf("normalization=%+v %v", got, err)
		}
		if _, ok, err := s.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, at, lifecycleReceipt(a, at, "consume")); err != nil || ok {
			t.Fatalf("invalid permit consumed=%v %v", ok, err)
		}
	}
}
