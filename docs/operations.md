# Running Draftcat unattended

This guide covers the settings that matter once Draftcat runs as a service:
preflight, schedules, failure handling, shutdown, spend warnings, metrics and
credential redaction.

## Preflight: `draftcat doctor`

```bash
draftcat doctor                    # ./config.yaml and ./skills
draftcat doctor --config /etc/draftcat/config.yaml --skills /etc/draftcat/skills --json
```

`doctor` checks everything the engine needs at start and says what to do about
each problem:

| Check | Fails when |
|---|---|
| config, validate | the file is unreadable or `draftcat validate` reports errors |
| telegram token, provider key | the configured environment variables are empty |
| operators | no `allowed_users` or no chat to reach them |
| relay | a relay is configured but its shared secret is empty |
| webhook | the listener is enabled but its secret is empty |
| state store | the file cannot be opened, or its directory is not writable |
| schedule | a schedule does not parse |

Warnings cover unsigned receipts (`DRAFTCAT_APPROVAL_SECRET` empty), a
`secrets.yaml` readable by other users, busy listener ports, unsettled provider
usage and approval gates left open. Each timer pipeline is listed with its next
run time in its own time zone.

`doctor` is read-only: it never creates or migrates the state store and never
calls a provider. It exits 1 when any check fails, so it can gate a deploy:

```bash
draftcat doctor && systemctl restart draftcat
```

## Schedules

```yaml
pipelines:
  - name: morning-digest
    schedule: "0 8 * * 1-5"      # weekdays 08:00
    timezone: Europe/Berlin      # IANA zone; default is the host's zone
    catch_up: true               # run a slot missed during downtime once at start
    pause_after_failures: 3      # pause after 3 failed runs in a row
    steps: [...]
  - name: inbox-sweep
    schedule: 30m                # interval
```

`schedule` accepts:

- an interval: any Go duration (`30m`, `4h`, `24h`);
- a five-field cron expression: minute, hour, day of month, month, day of week,
  with `*`, lists (`9,13`), ranges (`1-5`), steps (`*/15`) and names
  (`mon-fri`, `jan`). When day of month and day of week are both restricted,
  either one matching is enough, as in standard cron;
- `@hourly`, `@daily`, `@weekly`, `@monthly`;
- `manual` (operator `/run` only) or `webhook`.

Calendar schedules run at wall-clock time in `timezone`, across daylight-saving
changes: a slot inside a skipped hour runs once right after the jump.

Schedules continue from the run history in the state store. An interval
pipeline runs one interval after its last recorded run, so a `24h` pipeline
keeps its daily cadence across restarts and an overdue one runs at the next
tick. A calendar pipeline waits for its next slot; with `catch_up: true` a slot
that passed while the engine was down runs once at start.

`/cron` shows each pipeline's schedule, last run and next run.
`/cron set <name> <schedule>` accepts intervals and cron expressions and refuses
a schedule that does not parse.

## One run at a time

Every start of a pipeline — timer, `/run`, the Run-now button, a webhook — takes
the same claim. A pipeline has at most one run in progress: a second `/run`
answers `Not started: <name> (pipeline is already running)` and a second webhook
gets `409`. An operator's `/run` works while the pipeline's timer is paused;
webhooks and timers do not.

## Pausing a failing pipeline

With `pause_after_failures: N`, a timer pipeline that fails N runs in a row is
paused and the operator receives one message with the last error. Any
successful run resets the count. The count is rebuilt from the run history at
start, so a pipeline that was failing before a restart starts paused and the
operator is told again. `/cron resume <name>` clears the pause and the count.

## Shutdown

On `SIGINT` or `SIGTERM` the engine stops admitting runs, closes the webhook
listener after its in-flight requests, and waits for running pipelines:

```yaml
timeouts:
  shutdown_grace: 30s   # default; 0s exits at once
```

Approval taps keep arriving during the wait, so a run blocked on a decision can
still complete. Set your service manager's stop timeout above the grace period
(`TimeoutStopSec=` for systemd, `stop_grace_period:` for Compose). A gate still
open when the grace period ends is closed out and reported at the next start.

## Spend warnings

```yaml
budgets:
  per_day_tokens: 100000
  per_day_cost: 20
  alert_at: [0.5, 0.8, 0.95]
```

When today's usage (UTC) crosses a fraction of a daily cap, the operator
channel receives one message, for example
`[budget] 80% of today's cost cap used: 16.0400 of 20.0000 (UTC day 2026-10-08).`
Each threshold is sent once per day and cap, also across restarts. A jump past
several thresholds sends the highest.

## Metrics

With `observability.prometheus.enabled`, `/metrics` adds current-state gauges
next to the existing counters and histograms:

| Gauge | Meaning |
|---|---|
| `draftcat_pipelines_running` | runs in progress |
| `draftcat_pipeline_paused{pipeline}` | 1 when the timer is paused |
| `draftcat_pipeline_consecutive_failures{pipeline}` | current failure streak |
| `draftcat_approvals_open` | approval gates waiting on a decision |
| `draftcat_budget_day_tokens`, `draftcat_budget_day_tokens_limit` | today's tokens and the cap |
| `draftcat_budget_day_cost`, `draftcat_budget_day_cost_limit` | today's spend and the cap |
| `draftcat_budget_unsettled_calls` | provider calls awaiting reconciliation |

## Credential redaction

The values of every credential variable the engine reads — the Telegram token,
the provider key, the webhook, relay and approval secrets, the GoHighLevel key
and OTLP header values — are replaced by `[REDACTED:<VARIABLE>]` in the process
log, operator notifications, stored run and webhook errors, JSON spans and OTLP
exports. Values shorter than eight characters are not registered.

Approval drafts are shown to the operator exactly as they will be sent, so the
payload hash covers what the operator saw. Use a `model_policy` rule to hold or
refuse drafts that contain credentials.
