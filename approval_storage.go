package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
	"github.com/renezander030/draftcat/internal/redact"
	"github.com/renezander030/draftcat/internal/relay"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func persistApprovalReceipt(envelope statestore.ApprovalEnvelope) error {
	if state == nil {
		return nil
	}
	if os.Getenv("DRAFTCAT_APPROVAL_SECRET") != "" && (envelope.Nonce == "" || envelope.Signature == "") {
		return fmt.Errorf("signed approval receipt could not be created")
	}
	return state.RecordApprovalV2(envelope)
}

// recordSkip writes the receipt for a skip decision, naming the operator who
// skipped when the channel reports one, and the operator's reason as a
// separately signed note bound to that receipt.
func recordSkip(secret []byte, runID, pipeline string, step config.StepConfig, draft string,
	window time.Duration, quorumN int, decider int64, reason string) error {
	obs.RecordApproval(pipeline, step.Name, "skip")
	if state == nil {
		return nil
	}
	decidedAt := time.Now()
	sum := sha256.Sum256([]byte(draft))
	envelope := newApprovalEnvelope(secret, runID, pipeline, step,
		decidedAt, decidedAt.Add(window), "skip", decider, hex.EncodeToString(sum[:]),
		quorumN, 0, "human-approval")
	if err := persistApprovalReceipt(envelope); err != nil {
		return err
	}
	reason = strings.TrimSpace(redact.String(reason))
	if reason == "" {
		return nil
	}
	if len(reason) > relay.MaxReasonLen {
		reason = reason[:relay.MaxReasonLen]
	}
	note := statestore.ApprovalNote{ReceiptID: envelope.ReceiptID, OperatorID: decider, Reason: reason, NotedAt: decidedAt}
	if len(secret) > 0 {
		nonce, err := approval.NewNonce()
		if err != nil {
			return fmt.Errorf("sign skip reason: %w", err)
		}
		note.Nonce = nonce
		note.Signature = approval.SignNote(secret, approval.NoteFields{
			ReceiptID: note.ReceiptID, OperatorID: note.OperatorID, Reason: note.Reason, NotedAt: decidedAt.Unix(),
		}, nonce)
	}
	return state.RecordApprovalNote(note)
}
