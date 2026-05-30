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

## Endpoints

All endpoints require `Authorization: Bearer <token>` when `auth.method=bearer`.
All accept `Content-Type: application/json` and respond with `{"ok": true}` on
success.

| Endpoint                       | Direction | Purpose                                                           |
| ------------------------------ | --------- | ----------------------------------------------------------------- |
| `POST /voice/session_start`    | Dograh → draftcat | Call connected. Persists to `voice_sessions`.            |
| `POST /voice/event`            | Dograh → draftcat | Mid-call event (utterance, intent, tool call). `voice_events`. |
| `POST /voice/session_end`      | Dograh → draftcat | Call ended with outcome, transcript, cost. `voice_outcomes`. |
| `POST /voice/handoff`          | Dograh → draftcat | Transfer-to-human request. `voice_handoffs`. Response carries routing target. |
| `POST /voice/learning`         | Dograh → draftcat | Agent-flagged Learning-Item. `voice_learnings`.          |
| `POST /voice/pre_call_context` | Dograh → draftcat | Enrich greeting with account context. Sub-300ms p95 budget. |

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
| `dograh_staging_smoke`       | POST `/api/v1/public/agent/workflow/{workflow_uuid}` to `voice.dograh.staging_url`. Sets `data.voice_admin_smoke_run_id`. |
| `dograh_prod_publish`        | PUT `/api/v1/workflow/{workflow_id}` to `voice.dograh.base_url` with the JSON read from `vars.definition_path`. Dograh auto-versions on PUT, preserving prior versions. Sets `data.voice_admin_publish_status`. |

## Example pipelines

- [`fixtures/voice-dach-screener/pipeline.yaml`](../fixtures/voice-dach-screener/pipeline.yaml) — DACH screening (harvest > extract > approve > notify).
- [`fixtures/voice-dach-screener/guardrail.yaml`](../fixtures/voice-dach-screener/guardrail.yaml) — full 7-step Learning-Item review (harvest > group > approve > propose > approve-diff > commit > smoke > approve-deploy > publish > notify). Maps the case-study Guardrail flow onto draftcat's deterministic / ai / approval primitives plus the three admin actions above.

## State

The plugin reuses draftcat's existing SQLite state database (`state.db` by
default). On first start with `-tags voice`, five new tables are created:
`voice_sessions`, `voice_events`, `voice_outcomes`, `voice_handoffs`,
`voice_learnings`.

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
