package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

type storageApprovalChannel struct {
	stubApprovalChannel
	prompts, sends int
}

func (s *storageApprovalChannel) Send(string) error { s.sends++; return nil }
func (s *storageApprovalChannel) SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error) {
	s.prompts++
	return s.stubApprovalChannel.SendForApproval(ctx, draft, approvers)
}

func TestPipelineStorageFailureStopsRelease(t *testing.T) {
	for _, table := range []string{"pending_approvals", "action_approvals"} {
		t.Run(table, func(t *testing.T) {
			st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			old := state
			state = st
			t.Cleanup(func() { state = old; _ = st.Close() })
			_, err = st.DB().ExecContext(context.Background(), "CREATE TRIGGER reject_storage BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'storage unavailable'); END")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Timeouts: config.TimeoutConfig{OperatorApproval: "1s"}}
			ch := &storageApprovalChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 7}}
			p := config.PipelineConfig{Name: "pipeline", Steps: []config.StepConfig{{Name: "review", Type: "approval"}, {Name: "notify", Type: "deterministic", Action: "notify"}}}
			err = runPipeline(cfg, p, &BudgetTracker{dayStart: time.Now()}, ch, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("failure must stop release: %v", err)
			}
			if ch.sends != 0 {
				t.Fatal("pipeline executed after storage failure")
			}
			if table == "pending_approvals" && ch.prompts != 0 {
				t.Fatal("prompt sent before durable pending write")
			}
		})
	}
}

func TestAutomaticApprovalRequiresReceiptStorage(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := state
	state = st
	t.Cleanup(func() { state = old; _ = st.Close() })
	_, err = st.DB().ExecContext(context.Background(), "CREATE TRIGGER reject_receipts BEFORE INSERT ON action_approvals BEGIN SELECT RAISE(ABORT,'storage unavailable'); END")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Policy: config.ApprovalPolicy{AutoApprove: []config.AutoApproveRule{{Risk: config.RiskLow}}}}
	ch := &storageApprovalChannel{}
	p := config.PipelineConfig{Name: "pipeline", Steps: []config.StepConfig{{Name: "review", Type: "approval", Risk: config.RiskLow}, {Name: "notify", Type: "deterministic", Action: "notify"}}}
	if err := runPipeline(cfg, p, &BudgetTracker{dayStart: time.Now()}, ch, nil, nil); err == nil {
		t.Fatal("automatic approval ignored receipt failure")
	}
	if ch.sends != 0 {
		t.Fatal("automatic approval released next step")
	}
}

func TestModelReviewRequiresReceiptStorage(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old, oldCh := state, opChan
	state, opChan = st, &stubApprovalChannel{action: "approve", id: 7}
	t.Cleanup(func() { state, opChan = old, oldCh; _ = st.Close() })
	_, err = st.DB().ExecContext(context.Background(), "CREATE TRIGGER reject_receipts BEFORE INSERT ON action_approvals BEGIN SELECT RAISE(ABORT,'storage unavailable'); END")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Timeouts: config.TimeoutConfig{OperatorApproval: "1s"}, ModelPolicy: config.ModelPolicyConfig{Rules: []config.ModelPolicyRule{{ID: "review", Phase: "output", Pattern: "claim", Action: "review"}}}}
	if err := enforceModelPolicy(context.Background(), cfg, "drafter", "output", "claim"); err == nil {
		t.Fatal("model output released without durable approval receipt")
	}
}
