package main

import (
	"fmt"
	"os"

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
