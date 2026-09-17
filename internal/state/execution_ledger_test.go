package state

import (
	"testing"
	"time"
)

func TestToolActionIdempotencyAndConsumeOnce(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	a := ToolAction{
		ActionID: "send-1", Tool: "send_email", Agent: "agent", ArgsHash: "sha256:args",
		PolicyHash: "sha256:policy", BindingHash: "sha256:binding",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	got, created, err := s.ReserveToolAction(a)
	if err != nil || !created || got.Status != "pending" {
		t.Fatalf("reserve = %+v created=%v err=%v", got, created, err)
	}
	if _, created, err = s.ReserveToolAction(a); err != nil || created {
		t.Fatalf("matching retry created=%v err=%v", created, err)
	}
	drift := a
	drift.BindingHash = "sha256:drift"
	if _, _, err = s.ReserveToolAction(drift); err == nil {
		t.Fatal("binding drift under one action id was accepted")
	}
	if err = s.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ConsumeToolAction(a.ActionID, a.BindingHash, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("first consume ok=%v err=%v", ok, err)
	}
	if final, ok, err := s.ConsumeToolAction(a.ActionID, a.BindingHash, now.Add(2*time.Minute)); err != nil || ok || final.Status != "consumed" {
		t.Fatalf("second consume = %+v ok=%v err=%v", final, ok, err)
	}
}

func TestWebhookAdmissionSurvivesAndInterrupts(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	a := WebhookAdmission{ID: "wh_1", Pipeline: "p", BodyHash: "sha256:body", CreatedAt: now, UpdatedAt: now}
	if err := s.BeginWebhookAdmission(a); err != nil {
		t.Fatal(err)
	}
	if err := s.InterruptWebhookAdmissions(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.WebhookAdmission(a.ID)
	if err != nil || got.Status != "interrupted" || got.BodyHash != a.BodyHash {
		t.Fatalf("admission = %+v err=%v", got, err)
	}
}

func TestReconcilePreservesUnexpiredAllowedPermit(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	a := ToolAction{ActionID: "a", Tool: "send", ArgsHash: "h", PolicyHash: "p", BindingHash: "b",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, _, err := s.ReserveToolAction(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideToolAction("a", "allowed", "allow", "approved", "operator", now); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireToolActions(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ToolAction("a")
	if err != nil || got.Status != "allowed" {
		t.Fatalf("permit=%+v err=%v", got, err)
	}
}
