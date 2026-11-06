# Voice plugin

Draftcat's `voice` build tag adds an HTTP receiver for voice-AI session events
and three pipeline harvest actions that consume them. The orchestrator (Dograh,
Pipecat-direct, or any other) calls draftcat over webhooks. Draftcat owns
governance: per-day call budgets, schema-validated outcomes, operator approval
before any downstream write, and a 7-step Learning-Item review flow.

The plugin is intentionally orchestrator-agnostic: the 5 receivers accept any
JSON-posting caller. The reference integration target is Dograh
(github.com/dograh-hq/dograh), which wires up via its built-in `webhook` nodes.

## Building

```sh
go build -tags voice -o draftcat
go test  -tags voice ./...
```

Without the tag, the lean binary is unchanged.

## Configuration

Add a `voice` block to `config.yaml`. The bearer token must match what Dograh
sends in the `Authorization: Bearer <token>` header (configured in Dograh as a
`BEARER_TOKEN` webhook credential).

```yaml
voice:
  enabled: true
  listen_addr: 127.0.0.1:8080
  public_base_url: https://draftcat.client.eu
  auth:
    method: bearer
    token_env: DRAFTCAT_VOICE_TOKEN
  dograh:
    base_url: https://dograh.client.internal
    api_key_env: DOGRAH_API_KEY
  pre_call:
    enabled: true
    timeout_ms: 300
    routing_rules:
      - {if: support_tier == 'enterprise', workflow: dach-enterprise}
      - {if: lang == 'de-CH',              workflow: schweizerdeutsch-screener}
      - {default: standard-de}

budgets:
  per_day_calls: 200
  per_day_call_minutes: 1500
```

Run behind Caddy or nginx for TLS termination. Dograh requires HTTPS webhooks.

## Daily call caps

`budgets.per_day_calls` and `budgets.per_day_call_minutes` cap the voice calls
of one UTC day, like the other daily caps. Both are optional; 0 or absent = no
cap. A config reload applies new values to the next call.

What draftcat can know decides when each cap applies:

| When | What happens |
|---|---|
| `POST /voice/pre_call_context` | Refused with `429` once today's calls have reached `per_day_calls` or today's minutes have reached `per_day_call_minutes`. Nothing is counted here (the request carries no session id). This is the only synchronous call in the Dograh wiring, so it is where a Dograh workflow can stop a call before it runs. |
| `POST /voice/session_start` | The same check, and the call is counted, in one transaction. A refused call answers `429` and is not stored, so it never shows up as a completed call. A repeated `session_start` for a call already counted is accepted without counting it again. |
| `POST /voice/session_end` | The call's minutes are recorded: from its `session_start` time to this end time, rounded up to whole minutes. A call whose `session_start` never arrived is counted here, with no minutes (its length is unknown); the end of a refused call records nothing. |
| `dograh_staging_smoke` | The run places a call, so the action is refused once a cap is reached. The call itself is counted when its `session_start` reaches draftcat. |

A refusal answers `{"ok": false, "refused": true, "reason": "BUDGET_BLOCKED: daily voice call limit 200 reached (used: 200, UTC day 2026-11-06)"}`.
The orchestrator decides what to do with it; wire the workflow to end the call
on a non-2xx answer. When the caps are set and the usage ledger cannot be read,
the call is refused with `503` (`"call budget unavailable"`), because draftcat
does not admit a call it cannot count.

Minutes are known only when a call ends, so the minute cap stops new calls
once the minutes of ended calls reach it; calls already running finish and
their minutes can carry the day past the cap. A call's count and minutes land
on the UTC day it was admitted, even when it ends after midnight.

The operator gets one notice per cap and UTC day, on the first refusal:
`[budget] Daily voice call cap reached: 200 of 200 calls (UTC day 2026-11-06). New calls are refused until the next UTC day.`
A restart does not repeat it.

Usage is recorded in the same state database as the token and cost caps, so it
survives a restart, and both `/status` (`Calls today: 12 / 200`) and
`draftcat budget status` (text and `--json`, field `call_caps`) show it. Calls
are counted whether or not a cap is set; without a cap nothing is refused and a
failure to record is only logged.

Send `started_at_ms` with `session_start` and `ended_at_ms` with
`session_end` when the orchestrator posts its webhooks after the call rather
than as it happens: without them draftcat uses the arrival times, and the
minutes come out short.

## Endpoints

All endpoints require `Authorization: Bearer <token>` when `auth.method=bearer`.
All accept `Content-Type: application/json` and respond with `{"ok": true}` on
success.

| Endpoint                       | Direction | Purpose                                                           |
| ------------------------------ | --------- | ----------------------------------------------------------------- |
| `POST /voice/session_start`    | Dograh → draftcat | Call connected. Counts the call, or refuses it with `429` at a daily cap. Persists to `voice_sessions`. |
| `POST /voice/event`            | Dograh → draftcat | Mid-call event (utterance, intent, tool call). `voice_events`. |
| `POST /voice/session_end`      | Dograh → draftcat | Call ended with outcome, transcript, cost. `voice_outcomes`. Records the call's minutes. |
| `POST /voice/handoff`          | Dograh → draftcat | Transfer-to-human request. `voice_handoffs`. Response carries routing target. |
| `POST /voice/learning`         | Dograh → draftcat | Agent-flagged Learning-Item. `voice_learnings`.          |
| `POST /voice/pre_call_context` | Dograh → draftcat | Enrich greeting with account context. Sub-300ms p95 budget. Refused with `429` at a daily call cap. |

## Wiring Dograh

Dograh's `webhook` nodes execute asynchronously after a workflow run completes.
Place separate webhook nodes on different paths in your workflow so they reach
the right draftcat endpoint with the right payload.

Minimal workflow wiring (one node per draftcat endpoint that path needs):

| Workflow position           | Webhook target                              | Payload template includes                              |
| --------------------------- | ------------------------------------------- | ------------------------------------------------------ |
| First node (always reached) | `/voice/session_start`                      | `id`, `caller_phone`, `workflow`                       |
| Terminal node (always)      | `/voice/session_end`                        | `session_id`, `outcome`, `transcript`, `recording_url` |
| On handoff branch only      | `/voice/handoff`                            | `session_id`, `reason`                                 |
| On learning-flag branch     | `/voice/learning`                           | `session_id`, `description`, `category`, `severity`   |
| First node (HTTP custom)    | calls `/voice/pre_call_context` synchronously and stores result in `initial_context` |

Example payload for `/voice/session_end` (Dograh template):

```json
{
  "session_id": "{{workflow_run_id}}",
  "ended_at_ms": "{{call_time_ms}}",
  "outcome": {{gathered_context.outcome | tojson}},
  "transcript": {{transcript_json | tojson}},
  "recording_url": "{{recording_url}}",
  "cost_cents": {{cost_info.cost_cents}}
}
```

## Pipeline actions

Seven new actions are available once draftcat is built with `-tags voice`.

### Harvest (deduped by record ID per `(pipeline, scope)`)

| Action                    | Yields                                                  |
| ------------------------- | ------------------------------------------------------- |
| `voice_calls_completed`   | `data.voice_calls`, `data.voice_call_count`             |
| `voice_handoffs_pending`  | `data.voice_handoffs`, `data.voice_handoff_count`       |
| `voice_learnings_new`     | `data.voice_learnings`, `data.voice_learning_count`     |

### Resolve

| Action                    | Behavior                                                                                  |
| ------------------------- | ----------------------------------------------------------------------------------------- |
| `voice_handoffs_resolve`  | Reads `data.ai_output.resolutions[].{handoff_id, target}` and writes resolutions back to the voice store. Sets `data.voice_handoffs_resolved_count`. |

### Admin (used by the 7-step guardrail pipeline)

| Action                       | Behavior                                                                                                                |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `git_commit_workflow_update` | Optionally writes `data[content_var]` to `vars.path`, then `git add` + `git commit` in `vars.repo_dir`. Sets `data.voice_admin_commit_sha`. |
| `dograh_staging_smoke`       | POST `/api/v1/public/agent/workflow/{workflow_uuid}` to `voice.dograh.staging_url`. Sets `data.voice_admin_smoke_run_id`. Refused once a daily call cap is reached. |
| `dograh_prod_publish`        | PUT `/api/v1/workflow/{workflow_id}` to `voice.dograh.base_url` with the JSON read from `vars.definition_path`. Dograh auto-versions on PUT, preserving prior versions. Sets `data.voice_admin_publish_status`. |

## Example pipelines

- [`fixtures/voice-dach-screener/pipeline.yaml`](../fixtures/voice-dach-screener/pipeline.yaml) — DACH screening (harvest > extract > approve > notify).
- [`fixtures/voice-dach-screener/guardrail.yaml`](../fixtures/voice-dach-screener/guardrail.yaml) — full 7-step Learning-Item review (harvest > group > approve > propose > approve-diff > commit > smoke > approve-deploy > publish > notify). Maps the case-study Guardrail flow onto draftcat's deterministic / ai / approval primitives plus the three admin actions above.

## State

The plugin reuses draftcat's existing SQLite state database (`state.db` by
default). On first start with `-tags voice`, five new tables are created:
`voice_sessions`, `voice_events`, `voice_outcomes`, `voice_handoffs`,
`voice_learnings`. Call usage goes to the budget tables every build creates:
`budget_days` (the daily totals) and `budget_voice_calls` (one row per call, so
a repeated webhook is never counted twice).

## Smoke test

With draftcat running and a bearer token set:

```sh
export TOKEN="$DRAFTCAT_VOICE_TOKEN"
SID="smoke-$(date +%s)"

curl -sS -X POST http://127.0.0.1:8080/voice/session_start \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$SID\",\"caller_phone\":\"+491234567890\",\"workflow\":\"smoke\"}"

curl -sS -X POST http://127.0.0.1:8080/voice/session_end \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"outcome\":{\"decision\":\"qualified\",\"score\":85},\"cost_cents\":1200}"

# Next run of voice-dach-screener pipeline harvests the session and triggers
# operator approval on Telegram.
```
