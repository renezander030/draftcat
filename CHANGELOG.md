# Changelog

## 0.9.0

- Isolate pipeline token and cost totals and serialize model admission against settled daily usage.
- Persist daily usage by UTC date and preserve uncertain provider calls across restarts.
- Govern every engine model request, including classification and rewrites, and charge responses before output policy review.
- Require durable approval and receipt writes before releasing pipeline, model, or tool actions.
- Record immutable, action-bound external execution outcomes with authenticated completion and polling.
- Apply expiration and current policy checks consistently to live and recovered tool permits.
- Add authenticated revocation of pending and allowed tool actions with conditional transitions against consumption.
- Validate structured output and configured scalar schemas with exact numeric comparisons and explicit integer semantics.
- Bound provider response reads and stop automatic retries when billing is uncertain.

`draftcat budget status` inspects daily usage and unsettled calls; `draftcat budget reconcile` records verified provider usage after an interrupted call. See the [budget and lifecycle guide](docs/governance-lifecycle.md) for upgrade behavior, recovery, and caller-attested outcome semantics.

## 0.8.0

- Add durable, pipeline-scoped `Idempotency-Key` webhook retries. Matching bodies return the original admission; changed bodies are rejected.
- Claim signed webhook replay identities atomically, including concurrent requests.
- Upgrade older SQLite stores transactionally before indexing new columns.
- Refuse consume requests when the tool rule, operator channel, allowed reviewers, or approval timeout changed after the decision.
- Reject oversized and unreadable request bodies, unknown tool envelope fields, duplicate JSON keys, and trailing JSON.
- Preserve exact tool argument numbers for approval hashes and compare numeric policy bounds without rounding.
- Persist completed and failed pipeline runs with their run IDs; join approvals by exact identity while preserving legacy history.
- Add `draftcat receipts verify <file.jsonl|-> [--json]` to verify exported signed receipt fields without SQLite.
- Open audit and inspection commands in read-only mode, without creating or migrating databases.

Release packaging includes `--version`, synchronized npm metadata, and a tag workflow that tests before building native assets, publishing the container, and publishing npm after a native installer smoke test.

### Upgrade notes

Start the engine once to migrate an older state store before using read-only audit commands. Existing v1/v2 receipt signatures remain unchanged. Unconsumed permits from v0.7.0 require a new approval because the effective policy binding now includes the operator authorization configuration. Tool request envelopes are stricter and retain numeric token spelling; send the same spelling when retrying an action ID.
