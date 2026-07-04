# Checked Action Receipts

Draftcat treats every customer-facing action as a checked handoff: AI output is
validated, an operator approves or edits it, and deterministic code executes the
final action. A receipt records that boundary.

The receipt pattern matters because approval is not a prompt instruction. It is
a persisted fact that can be inspected later.

## Lifecycle

1. A pipeline proposes an outbound action.
2. Deterministic code validates the payload shape and policy preconditions.
3. The operator approves, edits, or rejects the action.
4. Draftcat records the decision and payload hash.
5. The approved action executes.
6. A receipt can be exported for audit or incident review.

## Current integrity layer

The approval package signs immutable decision fields with HMAC-SHA256:

- pipeline
- step
- decision time
- decision
- operator ID
- payload hash
- quorum requirement
- quorum result
- nonce

See [`internal/approval/receipt.go`](../internal/approval/receipt.go). If any
covered field changes after signing, verification fails.

## Receipt shape

The receipt should be explicit enough to answer:

- what did the agent propose?
- which payload was shown to the operator?
- who approved, edited, or rejected it?
- which policy checks passed?
- what actually executed?
- can the approval row still be verified?

Example:

```json
{
  "receipt_id": "act_20260704_001",
  "run_id": "run_abc123",
  "pipeline": "lead_reply",
  "step": "send_email",
  "action_type": "outbound_message",
  "status": "executed",
  "proposed_by": "agent",
  "approved_by": "operator:12345",
  "approved_at": "2026-07-04T09:30:00Z",
  "executed_at": "2026-07-04T09:31:00Z",
  "schema_version": 1,
  "payload_hash": "sha256:...",
  "signature_status": "valid",
  "policy_checks": [
    {"id": "recipient_allowlist", "status": "pass"},
    {"id": "budget_limit", "status": "pass"}
  ],
  "human_decision": {
    "decision": "edit_then_approve",
    "notes": "Tightened the CTA and removed an unsupported claim."
  }
}
```

## CLI direction

A small receipt surface should be enough for operators and auditors:

```bash
draftcat receipts list --run run_abc123
draftcat receipts show act_20260704_001
draftcat receipts export --format jsonl --out receipts.jsonl
```

## Design rule

Do not let the model decide whether the approval boundary was satisfied. The
model may draft an action, but Draftcat owns validation, approval persistence,
execution, and receipt verification.
