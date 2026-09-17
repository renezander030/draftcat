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

## Integrity layer

New decisions use receipt schema v2. The approval package signs immutable
decision fields with HMAC-SHA256:

- receipt, run, and action IDs
- pipeline
- step
- decision time
- decision
- operator ID
- payload hash
- quorum requirement
- quorum result
- policy and policy digest
- binding digest
- permit expiry and lifecycle
- nonce

See [`internal/approval/receipt.go`](../internal/approval/receipt.go). If any
covered field changes after signing, verification fails. Existing v1 receipts
continue to verify with their original canonical field set.

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
  "version": 2,
  "receipt_id": "rcpt_9d4d...",
  "run_id": "run_abc123",
  "action_id": "run_abc123:lead_reply:send_email",
  "pipeline": "lead_reply",
  "step": "send_email",
  "decision": "approve",
  "operator_id": 12345,
  "decided_at": "2026-09-17T09:30:00Z",
  "payload_hash": "sha256:...",
  "policy": "human-approval",
  "policy_hash": "sha256:...",
  "binding_hash": "sha256:...",
  "expires_at": "2026-09-17T13:30:00Z",
  "lifecycle": "decided",
  "quorum_n": 1,
  "quorum_got": 1,
  "nonce": "...",
  "signature": "...",
  "verification": "ok"
}
```

The SQLite row stores hashes and identifiers, not the customer payload.

## CLI

List recent receipts across all pipelines, or narrow to one pipeline:

```bash
draftcat receipts list --limit 100
draftcat receipts list --pipeline lead_reply --json
```

Inspect one receipt by its stable ID (legacy rows also accept their numeric row
ID):

```bash
draftcat receipts show rcpt_9d4d...
```

Export newline-delimited JSON in chronological order. Stdout makes it easy to
pipe into an auditor or log shipper; `--out` creates a mode `0600` file:

```bash
draftcat receipts export --pipeline lead_reply > receipts.jsonl
draftcat receipts export --out receipts.jsonl
```

Set `DRAFTCAT_APPROVAL_SECRET` while reading to receive `verification: ok` or
`tampered`. Signed rows without the key report `unverified`; unsigned rows
report `unsigned` explicitly.

## Design rule

Do not let the model decide whether the approval boundary was satisfied. The
model may draft an action, but Draftcat owns validation, approval persistence,
execution, and receipt verification.
