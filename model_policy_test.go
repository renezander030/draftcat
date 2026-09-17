package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestModelPolicyDenyIsAudited(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	prevState, prevChannel := state, opChan
	state, opChan = st, nil
	t.Cleanup(func() { state, opChan = prevState, prevChannel; _ = st.Close() })
	cfg := &config.Config{
		ModelPolicy: config.ModelPolicyConfig{
			Rules: []config.ModelPolicyRule{{
				ID: "secrets", Phase: "input", Pattern: `(?i)password`, Action: "deny", Reason: "secret-like input",
			}},
		},
	}
	ctx := withRun(context.Background(), "run-1", "pipeline")
	if err := enforceModelPolicy(ctx, cfg, "drafter", "input", "password=hidden"); err == nil {
		t.Fatal("matching input was not denied")
	}
	rows, err := st.ApprovalsForRun("run-1")
	if err != nil || len(rows) != 1 || rows[0].Version != 2 || rows[0].Decision != "policy_deny" || rows[0].BindingHash == "" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestModelPolicyReviewCanRelease(t *testing.T) {
	prev := opChan
	opChan = &stubApprovalChannel{action: "approve", id: 7}
	t.Cleanup(func() { opChan = prev })
	cfg := &config.Config{
		Timeouts: config.TimeoutConfig{OperatorApproval: "1s"},
		ModelPolicy: config.ModelPolicyConfig{
			Rules: []config.ModelPolicyRule{{
				ID: "review", Phase: "output", Pattern: `guarantee`, Action: "review", Reason: "claim review",
			}},
		},
	}
	if err := enforceModelPolicy(context.Background(), cfg, "drafter", "output", "we guarantee this"); err != nil {
		t.Fatalf("approved review blocked output: %v", err)
	}
}
