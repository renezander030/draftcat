# Changelog

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
