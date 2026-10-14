package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func hashCanonical(v interface{}) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newApprovalEnvelope(secret []byte, runID, pipeline string, step config.StepConfig,
	decidedAt, expiresAt time.Time, decision string, operatorID int64, payloadHash string,
	quorumN, quorumGot int, policy string) statestore.ApprovalEnvelope {
	actionID := strings.Trim(strings.Join([]string{runID, pipeline, step.Name}, ":"), ":")
	if actionID == "" {
		actionID = fmt.Sprintf("action:%d", decidedAt.UnixNano())
	}
	policyHash := hashCanonical(struct {
		Pipeline string            `json:"pipeline"`
		Step     config.StepConfig `json:"step"`
		Policy   string            `json:"policy"`
	}{pipeline, step, policy})
	bindingHash := hashCanonical(struct {
		Version     int    `json:"version"`
		ActionID    string `json:"action_id"`
		PayloadHash string `json:"payload_hash"`
		PolicyHash  string `json:"policy_hash"`
		ExpiresAt   int64  `json:"expires_at"`
	}{2, actionID, payloadHash, policyHash, expiresAt.Unix()})
	e := statestore.ApprovalEnvelope{
		ReceiptID: "rcpt_" + strings.TrimPrefix(newToolTicketID(), "tc_"),
		RunID:     runID, ActionID: actionID, Pipeline: pipeline, Step: step.Name,
		DecidedAt: decidedAt, Decision: decision, OperatorID: operatorID,
		PayloadHash: payloadHash, QuorumN: quorumN, QuorumGot: quorumGot,
		Policy: policy, PolicyHash: policyHash, BindingHash: bindingHash,
		ExpiresAt: expiresAt, Lifecycle: "decided",
	}
	if len(secret) == 0 {
		return e
	}
	signApprovalEnvelope(secret, &e)
	return e
}

// signApprovalEnvelope sets the envelope's nonce and v2 signature over
// e.FieldsV2(). It reports false, leaving the envelope unsigned, when no nonce
// could be drawn; callers decide whether an unsigned receipt is an error.
func signApprovalEnvelope(secret []byte, e *statestore.ApprovalEnvelope) bool {
	nonce, err := approval.NewNonce()
	if err != nil {
		return false
	}
	e.Nonce = nonce
	e.Signature = approval.SignV2(secret, e.FieldsV2(), nonce)
	return true
}
