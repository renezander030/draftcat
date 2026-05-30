<p align="center">
  <img src="logo.svg" alt="Draftcat" width="400">
</p>

<h1 align="center">Draftcat</h1>
<p align="center">Governed AI pipelines for service businesses — written in Go.</p>

<p align="center">
  <a href="https://github.com/renezander030/draftcat/stargazers"><img src="https://img.shields.io/github/stars/renezander030/draftcat?style=flat-square" alt="Stars"></a>
  <a href="https://github.com/renezander030/draftcat/blob/master/LICENSE"><img src="https://img.shields.io/github/license/renezander030/draftcat?style=flat-square" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go" alt="Go 1.24">
  <img src="https://img.shields.io/badge/release-v0.1-blueviolet?style=flat-square" alt="v0.1">
  <img src="https://img.shields.io/badge/build-passing-brightgreen?style=flat-square" alt="Build">
  <a href="docs/voice.md"><img src="https://img.shields.io/badge/voice%20AI-EU%20residency%20%C2%B7%20Dograh-00D4AA?style=flat-square" alt="Voice AI plugin (EU residency, Dograh)"></a>
</p>

Draftcat runs YAML-defined pipelines that triage email, qualify leads, draft replies, extract data from PDFs, and govern self-hosted **voice AI** deployments. Every outbound action passes through an operator approval gate; every LLM call is budget-checked; every fetched item is deduped against a SQLite state store. Single Go binary.

**AI suggests. Deterministic code decides. The operator signs off.**

## Voice AI plugin: self-hosted, EU residency, Dograh + Pipecat

Build with `-tags voice` to add the **EU-resident writeback + governance layer** for self-hosted voice agents (Dograh, Pipecat, or any orchestrator that posts JSON webhooks). Use cases: AI calling, inbound qualification, sales screening, support deflection in DE / AT / CH, with audio + transcripts never leaving the chosen EU region (STACKIT Frankfurt, PlusServer, IONOS, Hetzner, OVHcloud, Open Telekom Cloud, or a client-owned VPC).

- **5 lifecycle webhook receivers** matching the published writeback contract: `session_start`, `event`, `session_end`, `handoff`, `learning`
- **Pre-call context lookup** (sub-300ms p95) enriches the greeting from GHL / custom CRM HTTP before the agent speaks
- **3 harvest + 1 resolve pipeline action** feed completed calls, pending handoffs, and Learning-Items into the standard approval > extract > write flow
- **3 admin actions** drive Dograh's REST API directly: `git_commit_workflow_update`, `dograh_staging_smoke`, `dograh_prod_publish`
- **7-step Learning-Item review pipeline** ([`fixtures/voice-dach-screener/guardrail.yaml`](fixtures/voice-dach-screener/guardrail.yaml)) before any prompt / workflow / knowledge-base change reaches production
- **Per-day call + minute budgets** enforced at the engine level, parallel to the existing per-token caps
- **Bearer-token webhook auth** with constant-time compare
- **SQLite-backed audit** in the same state DB as the rest of draftcat

```sh
go build -tags voice -o draftcat
```

Reference orchestrator: [Dograh](https://github.com/dograh-hq/dograh). The 5-endpoint writeback contract is intentionally orchestrator-agnostic, so Pipecat-direct or any future alternative drops in without rewriting the writeback layer. Full wiring recipe + Dograh-side webhook node templates: [`docs/voice.md`](docs/voice.md). Runnable fixtures: [DACH screening](fixtures/voice-dach-screener/pipeline.yaml), [7-step guardrail](fixtures/voice-dach-screener/guardrail.yaml).

## Contents

- [Why Draftcat vs the alternatives](#why-draftcat-vs-the-alternatives)
- [Why this exists](#why-this-exists)
- [Quickstart](#quickstart) — lean + voice builds
- [How it works](#how-it-works)
- [Built-in actions](#built-in-actions) — including all `voice_*` and `dograh_*` actions
- [State & idempotency](#state--idempotency)
- [Governance](#governance)
- [Configuration](#configuration) — with `voice:` block example
- [Commands](#commands)

## Why Draftcat vs the alternatives

|                              | **Draftcat**                                    | **n8n**                                    | **LangChain agents**                |
| ---------------------------- | ---------------------------------------------- | ------------------------------------------ | ----------------------------------- |
| **Built for**                | AI-augmented business ops with governance      | General workflow automation                | Open-ended agent loops              |
| **AI execution model**       | Deterministic boundary; AI cannot fire actions | Bolt-on LLM nodes inside visual workflows  | Agent decides next action freely    |
| **Human-in-the-loop**        | Required on every outbound step                | Optional manual nodes                      | Optional; not the default           |
| **Token budgets**            | Per-step / pipeline / day, enforced            | None                                       | None                                |
| **Prompt-injection defense** | Input sanitization + output schema validation  | None                                       | None                                |
| **State & dedup**            | SQLite-backed; items processed at most once    | DB-backed                                  | In-memory                           |
| **Config format**            | YAML (code-reviewable, gitable)                | Visual editor + JSON export                | Python code                         |
| **Runtime**                  | Single Go binary                               | Node.js + Postgres                         | Python + dependency tree            |
| **Approval channels**        | Telegram, Slack (WIP)                          | Webhook / email                            | None built-in                       |
| **Voice AI integration**     | Built-in `voice` plugin: Dograh webhooks, pre-call lookup, 7-step Learning-Item guardrail | None                                       | None                                |
| **EU data residency**        | Self-hosted Go binary, deploys on STACKIT / PlusServer / IONOS / Hetzner / OVHcloud / Open Telekom Cloud / client VPC | Self-host possible                         | Self-host possible                  |

**Versus n8n**, Draftcat treats AI as the gated minority, not the default. n8n is a powerful no-code workflow tool with AI nodes bolted on; Draftcat is a code-first engine where every AI suggestion must pass schema validation and operator approval before it touches a customer. If you want drag-drop integrations across 400+ services, use n8n. If you want deterministic governance on a focused set of business ops, use Draftcat.

**Versus LangChain agents**, Draftcat refuses to let the LLM choose the next action. There is no agent loop — pipelines are fixed sequences of `deterministic` / `ai` / `approval` steps defined in YAML. The LLM produces structured output; the engine validates it against a schema; the operator approves it. LangChain is for research and open-ended exploration. Draftcat is for production systems where a wrong LLM choice means a real customer gets emailed.

## Why this exists

Service businesses run on volume communication — replying to leads, qualifying inbound, chasing stale deals, parsing invoices. AI handles the volume easily but is wrong often enough that you can't let it touch a customer without a human in the path. Draftcat is the smallest engine that makes that pattern routine: deterministic Go code does the fetching, routing, and dedup; AI does the drafting and classification under a token budget; an operator approves on Telegram or Slack before anything goes out. One business per instance, self-hosted, auditable.

![Demo](demo.gif)

## Quickstart

```bash
git clone https://github.com/renezander030/draftcat.git && cd draftcat
cp secrets.yaml.example secrets.yaml   # operator IDs + API keys
go build -o draftcat . && ./draftcat
```

Define pipelines in `config.yaml`, prompts in `skills/`. The engine opens a SQLite state store at `./state.db` on first boot.

**To enable the voice AI plugin** (Dograh webhook receivers, pre-call lookup, 7-step Learning-Item guardrail, EU residency wiring):

```bash
go build -tags voice -o draftcat . && ./draftcat
```

The lean binary is unchanged when the tag is off. See [`docs/voice.md`](docs/voice.md) for the full setup.

## How it works

Each pipeline is a sequence of typed steps:

| Step type        | What it does                                                          |
| ---------------- | --------------------------------------------------------------------- |
| `deterministic`  | Plain Go — fetch emails, parse PDFs, dedup, route, notify             |
| `ai`             | LLM inference with a skill template, budget-checked, schema-validated |
| `approval`       | Operator reviews via Telegram / Slack: approve / edit / reject        |

```yaml
pipelines:
  - name: invoice-due-diligence
    schedule: 1h
    steps:
      - name: parse-pdf
        type: deterministic
        action: pdf_extract
        vars: {path: /inbox/invoice.pdf}

      - name: extract-line-items
        type: ai
        skill: extract-line-items

      - name: verify-citations
        type: deterministic
        action: pdf_verify_cite
        vars: {fail_on_unresolved: "true"}

      - name: operator-review
        type: approval
        mode: hitl
        channel: telegram
```

## Built-in actions

| Action                       | What it does                                                              |
| ---------------------------- | ------------------------------------------------------------------------- |
| `gmail_unread`               | Fetch unread Gmail messages (deduped per pipeline)                        |
| `ghl_new_contacts`           | Fetch recent GoHighLevel contacts (deduped)                               |
| `ghl_stale_opportunities`    | Fetch stalled GHL opportunities (requires `vars.pipeline_id`)             |
| `ghl_unread_conversations`   | Fetch unread GHL conversations (deduped by `id + last_message_date`)      |
| `pdf_extract`                | Parse a PDF into text + per-fragment bounding boxes (pure-Go)             |
| `pdf_verify_cite`            | Resolve `<cite>` tags in AI output against the parsed PDF                 |
| `notify`                     | Send AI output to the operator channel                                    |
| `voice_calls_completed`      | Harvest completed voice calls from the Dograh writeback layer (`-tags voice`) |
| `voice_handoffs_pending`     | Harvest unresolved handoff requests for human routing (`-tags voice`)     |
| `voice_handoffs_resolve`     | Write `ai_output.resolutions[].{handoff_id, target}` back to the voice store (`-tags voice`) |
| `voice_learnings_new`        | Harvest agent-flagged Learning-Items for the 7-step review (`-tags voice`) |
| `git_commit_workflow_update` | Write `data[content_var]` to a workflow file + git add + commit (`-tags voice`) |
| `dograh_staging_smoke`       | POST to Dograh's outbound-trigger endpoint to smoke-test a workflow version (`-tags voice`) |
| `dograh_prod_publish`        | PUT a `workflow_definition` to Dograh prod (Dograh auto-versions on receipt) (`-tags voice`) |

Add a new action by appending a `case` to the deterministic switch in `main.go` and registering its name in `validate.go`. See `gohighlevel.go` for the connector pattern and `voice_actions.go` / `dograh_admin.go` for the build-tag-gated examples.

## State & idempotency

Draftcat persists cross-run state to SQLite at the path set in `config.yaml`:

```yaml
state:
  path: ./state.db   # default if omitted
```

- **Dedup.** Fetched item IDs (Gmail message IDs, GHL contact IDs, etc.) are recorded in `seen_items`. Subsequent runs skip already-processed items. Dedup is per `(pipeline, scope)` — the same item can be processed by two pipelines without interference.
- **Run history.** Every pipeline run records `pipeline / started_at / ended_at / status / error_text` to `pipeline_runs`.
- **Crash safety.** WAL mode + `synchronous=NORMAL` for durable writes without per-write fsync.

Items are marked seen at fetch time — once fetched, they won't be re-processed even if a downstream step fails. Replay manually via the `/run <pipeline>` operator command.

## Governance

- **Token budgets** — per-step / per-pipeline / per-day caps. Exceeding any budget halts the pipeline immediately.
- **Human-in-the-loop** — every outbound action requires explicit operator approval.
- **Input sanitization** — operator input is scrubbed for prompt-injection patterns before reaching the LLM.
- **Output validation** — AI output is validated against the skill's JSON schema. Invalid output is rejected.
- **Rate limiting** — per-user, per-minute limits on operator interactions.
- **Channel security** — allowed-user lists and input-length limits enforced at startup. Engine refuses to start without them.

## Configuration

```yaml
provider:
  type: openrouter
  api_key_env: OPENROUTER_API_KEY

models:
  haiku:       {model: anthropic/claude-haiku-4-5, max_tokens: 1024}
  gpt-4o-mini: {model: openai/gpt-4o-mini,         max_tokens: 1024}

budgets:
  per_step_tokens:     2048
  per_pipeline_tokens: 10000
  per_day_tokens:      100000
  per_day_calls:       200      # voice plugin
  per_day_call_minutes: 1500    # voice plugin

state:
  path: ./state.db
```

When built with `-tags voice`, an additional `voice:` block configures the webhook receivers, the Dograh REST endpoints used by the admin actions, and the pre-call context lookup:

```yaml
voice:
  enabled: true
  listen_addr: 127.0.0.1:8080
  public_base_url: https://draftcat.client.eu          # Dograh hits this
  auth:
    method: bearer
    token_env: DRAFTCAT_VOICE_TOKEN
  dograh:
    base_url: https://dograh.client.internal            # prod
    staging_url: https://dograh-staging.client.internal # for dograh_staging_smoke
    api_key_env: DOGRAH_API_KEY
  pre_call:
    enabled: true
    timeout_ms: 300
    lookups:
      - {source: ghl}                                   # auto-uses your existing GHL connector
      - {source: custom_http, url: https://crm.client.internal/lookup, header: "Authorization=Bearer {{env.CRM_TOKEN}}"}
    routing_rules:
      - {if: "support_tier == 'enterprise'", workflow: dach-enterprise}
      - {if: "lang == 'de-CH'",              workflow: schweizerdeutsch-screener}
      - {default: standard-de}
```

Skills are YAML prompt templates in `skills/`:

```yaml
# skills/classify-job.yaml
name: classify-job
system: |
  You are a job classifier. Given a job posting, return a JSON object with
  match (boolean), reason (one sentence), and score (0-100).
input_vars: [posting, profile]
output_schema:
  type: object
  required: [match, reason, score]
```

## Commands

```bash
draftcat                       # run the engine
draftcat validate [--strict]   # lint config + skills
draftcat test <pipeline>       # dry-run against fixtures/<pipeline>/
```

`draftcat test` walks the pipeline using JSON fixtures — never touches real APIs. AI steps return fixture text; approval steps auto-approve. See `fixtures/README.md`.

## Development

Pre-commit hooks (lefthook) run `gofmt`, `go vet`, `go build`, and `go test -short` in parallel on every staged Go file. Pre-push runs `draftcat validate --strict`.

```bash
make check       # fmt-check + vet + test-fast + build
make test-fast   # short test pass
```


## Patterns explained

The deterministic-boundary architecture is documented in the **Production AI Automation Notes** gist series. Each entry maps to specific draftcat code:

- [#1 Agent Approval Gates](https://gist.github.com/renezander030/9069db775e494ffd2cdd5a09adf83add) — proposed actions, schema validation, audit log
- [#2 Token Budgets](https://gist.github.com/renezander030/a7d99ad94b97f7943a9a04016d62faaa) — per-step, per-pipeline, per-day enforcement (`BudgetTracker` in `main.go`)
- [#5 SQLite Dedup + Crash Safety](https://gist.github.com/renezander030/8a23e32cde0c882a5aa069c4bfdf697f) — WAL mode, `seen_items`, run audit (`state.go`)
- [#6 Prompt-Injection Defense](https://gist.github.com/renezander030/213ffdf1ab1bdb169881927bc7080270) — input sanitization + output schema validation (`ChannelSecurity` + `skills/*.yaml`)
- [#7 PDF Cite Verification](https://gist.github.com/renezander030/7780cbc0b3ad4e802e8fba8bfc1c3a66) — auditable LLM extraction with per-fragment bounding boxes (`pdf.go`)

## Related projects

- [capcut-cli](https://github.com/renezander030/capcut-cli) — CLI to edit CapCut and JianYing video drafts. Same design DNA: single binary, no API needed, structured JSON boundary between agent and tool.

## Status

**v0.1** — early access. Single-business, single-operator deployments. Public APIs may change between minor versions until v1.0.

### Roadmap signals

Core engine:
- Webhook triggers (currently pipelines are scheduled-only)
- Generic HTTP action (drop-in for any REST integration not yet first-class)
- Per-step retry + circuit breaker
- Slack approval channel (Telegram is shipping today)
- Structured JSON logging + Prometheus metrics endpoint
- Google Sheets connector

Voice plugin v0.2 (planned):
- **Outbound calling campaigns** with consent gating (TCPA-aware in the US, DSGVO + UWG-aware in DE/AT/CH)
- **Recording redaction** (PII masking before audio + transcript hit persistent storage)
- **MCP bridge** so Dograh agents can call draftcat tools mid-conversation (parity with Dograh's recent MCP server release)
- **Real-Dograh integration verification** against a live instance once a paying deployment lands

Operational deliverables (separate repos, not in draftcat):
- `dograh-stackit` — Terraform + Ansible for reproducible STACKIT / PlusServer / IONOS / Hetzner / OVHcloud / Open Telekom Cloud deployment of the full voice stack (CPU VM for draftcat, GPU VM for Dograh + Whisper + vLLM + XTTS, 300 GB persistent model volume, Caddy TLS, smoke + warm-benchmark + attestation scripts)

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=renezander030/draftcat&type=Date)](https://star-history.com/#renezander030/draftcat&Date)

## License

MIT. See [LICENSE](LICENSE).
