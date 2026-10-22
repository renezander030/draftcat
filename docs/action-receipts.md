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
- permit expiry
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

Verify exported records without opening SQLite:

```bash
draftcat receipts verify receipts.jsonl --json
cat receipts.jsonl | draftcat receipts verify -
```

This requires `DRAFTCAT_APPROVAL_SECRET`, checks each signed v1/v2 field set,
and returns nonzero for unsigned or tampered rows. It does not certify export
completeness or unsigned `lifecycle` metadata. See the [reliability guide](reliability.md).

## Anchoring the receipt history

Each receipt proves its own fields. An anchor proves the history as a whole:
that no receipt was deleted, inserted or moved since the anchor was taken.

The anchor is the end of a SHA-256 hash chain over every stored receipt, in
the order the store appended them (ascending `action_approvals` row id, which
only grows and is never reused):

```text
link_0 = sha256("draftcat-receipt-chain/v1")
link_n = sha256(link_{n-1} || row_n)
```

`row_n` is the receipt's full stored row in a length-prefixed canonical form
(`row/1`): row id, receipt version and ID, run and action IDs, pipeline, step,
decision time, decision, operator, payload hash, quorum, nonce, signature,
policy, policy and binding hashes, expiry and lifecycle. Signed and unsigned
receipts are covered alike, so the chain also binds fields outside the
signature such as `lifecycle`. Receipts, their signatures and the schema are
unchanged; the chain is computed when you ask for it.

Take an anchor and keep it outside the store (a ticket, a second disk, a
signed email):

```bash
draftcat receipts anchor
#   receipts      1284
#   first         rcpt_0c1f...
#   last          rcpt_9d4d...
#   last decided  2026-10-21T17:02:11Z
#   anchor        sha256:5be1...
draftcat receipts anchor --json > anchor-2026-10-21.json
```

The JSON form adds a `trail`: each anchored receipt ID with the first 16 hex
digits of its link. With the trail, a check names the first position where the
history diverges; with only the `sha256:` link it says whether the history
still reproduces it.

Check the store against an anchor:

```bash
draftcat receipts verify-anchor anchor-2026-10-21.json
#   anchor ok: 1284 receipt(s) match the anchor; 37 appended since (sha256:5be1...)
draftcat receipts verify-anchor sha256:5be1... --json
draftcat audit-verify lead_reply --anchor anchor-2026-10-21.json
```

The chain is recomputed over the first `count` stored receipts (for a bare
link, over the prefix whose link matches). Receipts appended after the anchor
pass and are reported as a count. A mismatch exits 1 and names the position and
what happened there:

| kind | meaning |
| --- | --- |
| `deleted` | the anchored receipt at this position is no longer in the store |
| `inserted` | the receipt now at this position was not in the anchored history |
| `reordered` | both receipts exist, at other positions than anchored |
| `changed` | the same receipt holds different content than when anchored |

`audit-verify --anchor` runs the per-pipeline signature check as before and the
anchor check over the whole store; it exits 1 if either fails. Both commands
open the store read-only and never create or migrate it; `verify-anchor` needs
no `DRAFTCAT_APPROVAL_SECRET`. A malformed anchor exits 2.

## Skip reasons

An operator can say why an action was skipped: **Skip with reason...** on the
Telegram prompt, or `reason` on a `skip` decision from a hitl/v0 relay. The skip
is decided when the button is tapped (on a quorum gate it vetoes at once); the
reason follows as a text reply and is optional: without one within ten minutes
the skip stands on its own.

The skip receipt names the operator who skipped. The reason is stored as a
separate note bound to that receipt ID and signed with the same
`DRAFTCAT_APPROVAL_SECRET` (`note/1` envelope), so v2 receipts, exports and
proofs are unchanged and the note carries its own verification:

```bash
draftcat runs invoices
#   release-payment      skip        by 111 (0/2) [signed]
#                        reason: Wrong invoice amount [ok]
draftcat receipts show rcpt_9d4d...   # includes "note": {reason, operator_id, verification}
```

Reasons are capped at 500 bytes and pass through credential redaction. They are
the operator's own words; the draft itself is still stored only as a hash.

## Design rule

Do not let the model decide whether the approval boundary was satisfied. The
model may draft an action, but Draftcat owns validation, approval persistence,
execution, and receipt verification.
