# Changelog

## 0.11.0

- Record why an action was skipped: **Skip with reason...** on Telegram prompts and an optional `reason` on hitl/v0 `skip` decisions. The skip receipt names the operator; the reason is a separately signed note bound to the receipt, shown by `draftcat runs` and `draftcat receipts show` with its own verification.
- Reload `config.yaml` and `skills/` without a restart on `SIGHUP` or `/reload`. Pipelines, models, roles, budgets, timeouts and policies apply to runs that start afterwards; runs in progress keep their configuration. Invalid files are refused and the running configuration stays in place.

See [skip reasons](docs/action-receipts.md#skip-reasons) and [reloading the configuration](docs/operations.md#reloading-the-configuration).

### Upgrade notes

The state store gains an `approval_notes` table at first start. v2 receipts, exports and proofs are unchanged. Relays that do not send `reason` keep working. Telegram approval prompts carry a second row with **Skip with reason...**.

## 0.10.0

Operations you can leave running: calendar schedules, a preflight, a clean shutdown and credentials that stay out of every log.

- Add `draftcat doctor`, a read-only preflight of config, credentials, operator access, the state store, listener ports and schedules, with a fix for each finding and `--json` output. Exits 1 when a check fails.
- Schedule pipelines with five-field cron expressions and `@hourly`/`@daily`/`@weekly`/`@monthly`, evaluated in a per-pipeline `timezone`. `catch_up: true` runs a slot missed during downtime once at start.
- Continue interval schedules from the recorded run history across restarts.
- Admit every pipeline run, whether from the timer, `/run`, the Run-now button or a webhook, through one claim, keeping one run per pipeline in progress.
- Pause a timer pipeline after `pause_after_failures` consecutive failed runs and notify the operator once; the streak is restored at start and `/cron resume` clears it.
- Drain on `SIGINT`/`SIGTERM`: refuse new runs, close the webhook listener and wait up to `timeouts.shutdown_grace` (default 30s) for running pipelines.
- Notify the operator at `budgets.alert_at` fractions of the daily token and cost caps, once per threshold and UTC day, also across restarts.
- Replace configured credentials with `[REDACTED:<VARIABLE>]` in logs, operator notifications, stored run and webhook errors, JSON spans and OTLP exports.
- Export current-state Prometheus gauges: running pipelines, paused pipelines, failure streaks, open approvals, and today's tokens and spend against the caps.

`/cron set` accepts cron expressions. See [running Draftcat unattended](docs/operations.md).

### Upgrade notes

All new settings are optional. Configurations without them behave as before, except that interval pipelines continue from their last recorded run instead of starting a fresh interval at boot, and the engine waits up to 30 seconds for running pipelines on shutdown (`timeouts.shutdown_grace: 0s` restores an immediate exit). An operator `/run` of a pipeline that is already running is refused instead of starting a second run.

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
