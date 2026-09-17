package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/renezander030/draftcat/internal/config"
)

func modelPolicyPreview(text string, max int) string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}

func enforceModelPolicy(ctx context.Context, cfg *config.Config, role, phase, text string) error {
	rule, err := cfg.ModelPolicy.Match(role, phase, text)
	if err != nil {
		return err
	}
	if rule == nil {
		return nil
	}
	action := strings.ToLower(strings.TrimSpace(rule.Action))
	decision := "policy_deny"
	var operatorID int64
	expires := time.Now()
	if action == "review" {
		if opChan == nil {
			recordModelPolicyDecision(ctx, cfg, role, phase, text, rule, decision, operatorID, expires)
			return fmt.Errorf("model policy %q requires review but no operator channel is running", rule.ID)
		}
		timeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
		if timeout <= 0 {
			timeout = 4 * time.Hour
		}
		expires = time.Now().Add(timeout)
		msg := fmt.Sprintf("[draftcat] Model %s requires review\n\nrule: %s\nrole: %s\nreason: %s\n\n%s",
			phase, rule.ID, role, rule.Reason, modelPolicyPreview(text, cfg.ModelPolicy.PreviewLimit()))
		reviewCtx, cancel := context.WithTimeout(ctx, timeout)
		dec, reviewErr := opChan.SendForApproval(reviewCtx, msg, nil)
		cancel()
		if reviewErr == nil && dec.Action == "approve" {
			decision, operatorID = "approve", dec.ApproverID
			recordModelPolicyDecision(ctx, cfg, role, phase, text, rule, decision, operatorID, expires)
			return nil
		}
		if reviewErr != nil {
			decision = "timeout"
		} else if dec.Action != "" {
			decision = dec.Action
		}
	}
	recordModelPolicyDecision(ctx, cfg, role, phase, text, rule, decision, operatorID, expires)
	return fmt.Errorf("model %s blocked by policy %q: %s", phase, rule.ID, rule.Reason)
}

func recordModelPolicyDecision(ctx context.Context, cfg *config.Config, role, phase, text string,
	rule *config.ModelPolicyRule, decision string, operatorID int64, expires time.Time) {
	if state == nil {
		return
	}
	sum := sha256.Sum256([]byte(text))
	payloadHash := hex.EncodeToString(sum[:])
	step := config.StepConfig{Name: "model-" + phase + ":" + rule.ID, Type: "approval", Role: role, Risk: config.RiskHigh}
	policy := rule.ID + ": " + rule.Reason
	envelope := newApprovalEnvelope([]byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET")),
		runIDFromContext(ctx), pipelineFromContext(ctx), step, time.Now(), expires,
		decision, operatorID, payloadHash, 1, boolCount(decision == "approve"), policy)
	if err := state.RecordApprovalV2(envelope); err != nil {
		return
	}
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}
