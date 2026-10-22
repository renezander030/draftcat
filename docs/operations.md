# Running Draftcat unattended

This guide covers the settings that matter once Draftcat runs as a service:
preflight, schedules, failure handling, shutdown, spend warnings, metrics,
oversight reports and credential redaction.

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

### Checking the credentials themselves: `--online`

```bash
draftcat doctor --online
```

`--online` adds read-only calls that confirm each remote accepts what is
configured. Nothing is posted to the operator and no tokens are spent.

| Check | Call | Fails when |
|---|---|---|
| telegram api | Bot API `getMe` | the token is rejected or Telegram is unreachable |
| telegram chat | Bot API `getChat` | the bot cannot reach the operator chat (no conversation started, not in the group) |
| provider api | OpenRouter `GET /key`, other providers `GET /models` | the key is rejected (HTTP 401/403) or the endpoint is unreachable |
| relay reachable | `GET relay.url` | the relay does not answer at all |

Each probe has a 10 second limit. Credentials are redacted from every detail
line, including transport errors that quote a request URL.

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

## When a run fails

The operator notice for a failed run names the step it stopped in and every
step after it, so it is clear which actions did not happen:

```
[draftcat] ERROR in replies: [step:classify] LLM call failed: ...
halted at: classify
not run: review, send-reply, log-crm
```

The stored run error (`draftcat runs`) and the log keep the step's own error.

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

## Reloading the configuration

```bash
kill -HUP $(pidof draftcat)      # or /reload in the operator chat
systemctl reload draftcat        # with ExecReload=/bin/kill -HUP $MAINPID
```

A reload re-reads `config.yaml` and `skills/` and applies `pipelines`, `models`,
`roles`, `budgets`, `timeouts`, `model_policy` and `approval_policy` to every run
that starts afterwards. Runs in progress keep the configuration they started
with. Pause state, failure streaks and run history carry over; a pipeline whose
schedule changed is replanned from its last run, a removed pipeline stops being
scheduled, and a new one starts on its schedule.

The file is validated first. A file with errors is refused, the running
configuration stays in place, and the operator is told why. Sections that built
long-lived resources at start (`telegram`, `relay`, `provider`, `state`,
`webhook`, `tool_gate`, `observability`, `gmail`, `gohighlevel`, `voice`) keep
their running values; the reload message names any of them that changed in the
file, so you know a restart is needed for those.

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

`draftcat budget status` splits today's settled model calls by provider model
and by pipeline, largest cost first (`by_model` and `by_pipeline` in `--json`):

```
2026-10-15 UTC: 48210 tokens, 0.412000 cost, 0 voice calls, 0 call minutes
By model:
  anthropic/claude-sonnet-5.5                 12 call(s)     40110 tokens     0.371000 cost
  anthropic/claude-haiku-5.5                  31 call(s)      8100 tokens     0.041000 cost
By pipeline:
  replies                                     18 call(s)     39020 tokens     0.355000 cost
  inbox-triage                                25 call(s)      9190 tokens     0.057000 cost
```

Calls settled before this attribution existed are listed as `(unattributed)`.

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

`draftcat_otlp_exports_dropped_total` counts OTLP span exports that failed and were dropped; it appears after the first drop, next to the `[obs] OTLP export dropped` log line.

## Oversight report

```bash
draftcat report                          # the last 30 days
draftcat report --since 2026-09-01 --until 2026-09-30 --pipeline invoices
draftcat report --json > oversight-2026-09.json
```

`report` summarizes who decided what over a period, from the receipts in the
state store: decisions per pipeline and step, how many a person made and how
many configured policy released, decisions per operator, how many skips carry a
recorded reason, the tool executions harnesses reported, and the signature
status of every receipt. It ends with the first and last receipt id and a
SHA-256 over the receipt ids in decided order, so a `receipts export` of the
same window can be matched to it. It is read-only and exits 1 when a receipt
fails its signature check.

## Credential redaction

The values of every credential variable the engine reads — the Telegram token,
the provider key, the webhook, relay and approval secrets, the GoHighLevel key
and OTLP header values — are replaced by `[REDACTED:<VARIABLE>]` in the process
log, operator notifications, stored run and webhook errors, JSON spans and OTLP
exports. Values shorter than eight characters are not registered.

Approval drafts are shown to the operator exactly as they will be sent, so the
payload hash covers what the operator saw. Use a `model_policy` rule to hold or
refuse drafts that contain credentials.
