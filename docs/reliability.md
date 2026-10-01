# Reliable requests and audit inspection

## Retry a webhook without repeating the action

Choose one opaque key for one intended pipeline execution and keep it for every retry:

```bash
curl -X POST https://draftcat.example/hooks/invoice-due-diligence \
  -H "Authorization: Bearer $DRAFTCAT_WEBHOOK_SECRET" \
  -H "Idempotency-Key: invoice-4821" \
  -d '{"invoice":"4821"}'
```

The key is scoped to the pipeline and must be 1–128 URL-safe characters (letters,
digits, underscore, hyphen, dot, or colon). SQLite stores its hash and the exact
body hash. Matching retries return HTTP 202 with the original `admission_id`,
current status, and poll path even after completion or restart. A changed body
returns 409. Missing keys retain the previous behavior. Retain the key while the
admission exists; keys do not automatically expire. A retry never resumes an
interrupted admission: inspect it and explicitly choose a fresh key for a new run.

Bearer authentication is checked on every retry. When signatures are enabled,
every retry also needs an authentic signature within the clock-skew window.
A matching, already-admitted retry may reuse the original signature while it is
still timely; it only reads the existing admission. Starting new work claims the
signature atomically. Reusing that signature for another admission is refused.

## Send complete, unambiguous tool requests

Oversized HTTP request bodies return 413 instead of being truncated; unreadable
bodies return 400. `webhook.max_body_bytes` applies to webhook and tool-gate POSTs.
The tool gate also limits direct requests to 1 MiB and consume bodies to 4 KiB.
Unknown tool envelope fields, duplicate keys (including nested arguments), and
trailing JSON, and nesting above 128 levels return 400 before creating or consuming a permit. Webhook payloads
remain arbitrary bytes; the strict JSON envelope rules apply to the tool gate.

Tool argument numbers retain their JSON spelling, including integers above 2^53.
Distinct large integers therefore cannot share a rounded approval hash. Use the
same numeric spelling on an action ID retry: `1`, `1.0`, and `1e0` bind different
representations. Numeric min/max constraints compare exact decimal values and
reject non-finite strings such as `NaN`. YAML numeric bounds keep their existing
float64 configuration type; comparisons retain the supplied argument's exact digits.

## Reapprove after a policy change

Consumption compares the saved policy digest with the current effective tool
policy, including operator-channel settings, permitted reviewers, and the
approval window. A mismatch returns 409 without an execution permit. Request a
new action ID and obtain a fresh approval. This check also applies to permits
loaded after restart. Upgrading from v0.7.0 changes the policy digest, so obtain
fresh approval for any unconsumed permits.

## Upgrade and inspect state

Engine startup migrates older SQLite stores in one transaction. Indexes on new
columns are created after those columns exist, and previous receipt signatures
are retained. Completed and failed pipeline runs are recorded with the run ID
used by their approvals. `draftcat runs --json` includes `run_id` for new runs;
identity-less historical records use the old timestamp join, restricted to
identity-less approvals.

`runs`, `pending`, `receipts list|show|export`, and `audit-verify` open existing
state in read-only mode. A missing path fails without creating a new database.
Start the engine once to migrate an older store before inspecting it. Inspection
can read a live WAL-backed database; it does not run schema migrations.

## Verify exported receipt fields offline

```bash
draftcat receipts export --out receipts.jsonl
draftcat receipts verify receipts.jsonl --json
cat receipts.jsonl | draftcat receipts verify -
```

Set `DRAFTCAT_APPROVAL_SECRET` to the same signing key used by the instance.
The verifier opens no state database and recomputes each v1/v2 signature instead
of trusting the export's `verification` field. Exit 0 means all listed receipts
have valid signatures; unsigned or tampered rows return 1; a missing signing key
or invalid command returns 2. Malformed, oversized, empty, or unsupported-version
input fails. The JSON report includes line number, receipt ID, and verdict.

HMAC verification requires the signing secret. It verifies the fields covered by
the corresponding receipt version; it does not attest that a JSONL file is
complete, prove actual delivery, or authenticate unsigned display metadata such
as `lifecycle`. Keep the secret within the trusted operator boundary. Use
`zk-receipt` for the separate experimental privacy-preserving proof flow.
