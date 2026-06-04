<p align="center">
  <img src="logo.png" alt="Draftcat" width="440">
</p>

<p align="center"><b>Governed AI pipelines for service businesses — one Go binary.</b></p>

<p align="center">
  <a href="https://github.com/renezander030/draftcat/stargazers"><img src="https://img.shields.io/github/stars/renezander030/draftcat?style=flat-square" alt="Stars"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/renezander030/draftcat?style=flat-square" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go" alt="Go 1.24">
  <a href="docs/voice.md"><img src="https://img.shields.io/badge/voice%20AI-EU%20residency%20%C2%B7%20Dograh-00D4AA?style=flat-square" alt="Voice AI plugin"></a>
</p>

> **AI suggests. Deterministic code decides. The operator signs off.**

Draftcat runs YAML-defined pipelines that triage email, qualify leads, draft replies, extract data from PDFs, and govern self-hosted voice AI. Every outbound action passes an operator approval gate, every LLM call is budget-checked, and every fetched item is deduped against a SQLite state store. One business per instance, self-hosted, auditable.

![Demo](demo.gif)

## Quickstart

```bash
git clone https://github.com/renezander030/draftcat.git && cd draftcat
cp secrets.yaml.example secrets.yaml   # operator IDs + API keys
go build -o draftcat . && ./draftcat
```

Pipelines live in `config.yaml`, prompts in `skills/`. A SQLite store opens at `./state.db` on first boot. To add the EU-resident **voice AI** plugin: `go build -tags voice -o draftcat .` — the lean binary is unchanged when the tag is off. See [docs/voice.md](docs/voice.md).

## How it works

Each pipeline is a fixed sequence of typed steps. The LLM never chooses the next action — it produces structured output, the engine validates it against a schema, and an operator approves before anything reaches a customer.

| Step type       | What it does                                                          |
| --------------- | -------------------------------------------------------------------- |
| `deterministic` | Plain Go — fetch emails, parse PDFs, dedup, route, notify            |
| `ai`            | LLM inference with a skill template, budget-checked, schema-validated |
| `approval`      | Operator reviews via Telegram / Slack: approve / edit / reject       |

```yaml
pipelines:
  - name: invoice-due-diligence
    schedule: 1h
    steps:
      - {name: parse-pdf, type: deterministic, action: pdf_extract, vars: {path: /inbox/invoice.pdf}}
      - {name: extract,   type: ai,            skill: extract-line-items}
      - {name: verify,    type: deterministic, action: pdf_verify_cite, vars: {fail_on_unresolved: "true"}}
      - {name: review,    type: approval,      mode: hitl, channel: telegram}
```

A pipeline's `schedule` decides when it runs: an interval (`30m`, `1h`), `manual` (operator `/run` only), or `webhook` (an authenticated HTTP trigger — see [Triggers](#triggers)). Whichever fires it, the steps and the approval gate are identical.

## Why Draftcat

|                              | **Draftcat**                                   | **n8n**                               | **LangChain agents**             | **Agent harnesses** (Flue, Claude Code) |
| ---------------------------- | ---------------------------------------------- | ------------------------------------- | -------------------------------- | --------------------------------------- |
| **Built for**                | AI-augmented business ops with governance      | General workflow automation           | Open-ended agent loops           | Programmable autonomous agents          |
| **AI execution model**       | Deterministic boundary; AI cannot fire actions | Bolt-on LLM nodes in visual workflows | Agent decides next action freely | Agent acts autonomously in a sandbox    |
| **Human-in-the-loop**        | Required on every outbound step                | Optional manual nodes                 | Optional; not the default        | Optional (dispatch a message mid-run)   |
| **Token budgets**            | Per-step / pipeline / day, enforced            | None                                  | None                             | App-managed, not built in               |
| **Prompt-injection defense** | Input sanitization + output schema validation  | None                                  | None                             | Sandbox isolation; app-managed          |
| **State & dedup**            | SQLite-backed; items processed at most once    | DB-backed                             | In-memory                        | Session store / Durable Objects         |
| **Runtime**                  | Single Go binary                               | Node.js + Postgres                    | Python + dependency tree         | TypeScript, runtime-agnostic            |
| **Voice AI**                 | Built-in `voice` plugin (Dograh, EU residency) | None                                  | None                             | None                                    |

Use n8n for drag-drop integrations across 400+ services. Use LangChain for research and open-ended exploration. Use an agent harness like [Flue](https://github.com/withastro/flue) when you want an agent to roam a sandbox and choose its own steps. Use Draftcat when a wrong LLM choice means a real customer gets emailed.

Draftcat and agent harnesses sit at opposite ends of the same axis. A harness hands the model a sandbox and lets it decide what to do; Draftcat fixes the sequence in YAML and lets the model only fill in structured output that deterministic Go then validates and an operator approves. Both converge on the same authoring style — most logic lives in Markdown/YAML skills, not code — which is why Draftcat keeps its prompts in `skills/` and reads `AGENTS.md`-style context. The difference is purely where the decision boundary sits: for outbound business actions, Draftcat keeps it on the deterministic side.

## Built-in actions

| Action                       | What it does                                                              |
| ---------------------------- | ------------------------------------------------------------------------ |
| `gmail_unread`               | Fetch unread Gmail messages (deduped per pipeline)                       |
| `ghl_new_contacts`           | Fetch recent GoHighLevel contacts (deduped)                             |
| `ghl_stale_opportunities`    | Fetch stalled GHL opportunities                                          |
| `ghl_unread_conversations`   | Fetch unread GHL conversations                                          |
| `pdf_extract`                | Parse a PDF into text + per-fragment bounding boxes (pure-Go)           |
| `pdf_verify_cite`            | Resolve `<cite>` tags in AI output against the parsed PDF               |
| `notify`                     | Send AI output to the operator channel                                  |
| `voice_*` / `dograh_*`       | Harvest calls, handoffs, Learning-Items + drive Dograh's REST API (`-tags voice`) |

Add an action by appending a `case` to the deterministic switch in `main.go` and registering its name in `internal/validate/`. See `internal/ghl/ghl.go` and `internal/dograh/dograh.go` for connector patterns, `internal/voicebridge/` for the build-tag-gated voice action dispatch.

## Governance

- **Token budgets** — per-step / per-pipeline / per-day caps. Exceeding any budget halts the pipeline immediately.
- **Human-in-the-loop** — every outbound action requires explicit operator approval.
- **Input sanitization** — operator input is scrubbed for prompt-injection patterns before reaching the LLM.
- **Output validation** — AI output is validated against the skill's `output_schema` — field types, numeric `min`/`max`, and `enum` membership — and rejected before it can reach a downstream step.
- **Rate limiting** — per-user, per-minute limits on operator interactions.
- **Channel security** — allowed-user lists and input-length limits enforced at startup; the engine refuses to start without them.
- **Observability** — opt-in structured JSON spans, one per pipeline and one per step (duration, status, tokens, cost). Off by default; enable with `observability.spans: true` or `DRAFTCAT_TRACE=1`. One line per span, ready to pipe into a log collector or a future OpenTelemetry exporter.

## State & idempotency

State persists to SQLite at the path set in `config.yaml` (`./state.db` by default).

- **Dedup** — fetched item IDs are recorded per `(pipeline, scope)`; subsequent runs skip already-processed items. Marked seen at fetch time, so a downstream failure won't cause reprocessing. Replay via the `/run <pipeline>` operator command.
- **Run history** — every run records `pipeline / started_at / ended_at / status / error_text`.
- **Crash safety** — WAL mode + `synchronous=NORMAL` for durable writes without per-write fsync.

## Triggers

Besides interval and `manual` schedules, a pipeline can be triggered by an authenticated HTTP request. Set its `schedule: webhook` and enable the opt-in server:

```yaml
webhook:
  enabled: true
  addr: 127.0.0.1:8088     # bind locally; front with your reverse proxy
  secret_env: DRAFTCAT_WEBHOOK_SECRET   # required — bearer token
```

```bash
curl -X POST http://127.0.0.1:8088/hooks/invoice-due-diligence \
  -H "Authorization: Bearer $DRAFTCAT_WEBHOOK_SECRET" \
  -d '{"path": "/inbox/invoice.pdf"}'
```

The request body is passed to the pipeline as `{{webhook_body}}` / `{{input}}`. The trigger is rejected with `409` if that pipeline is already running, so a noisy caller can't start overlapping runs. The server opens no port unless `enabled`, refuses to start without a secret, and compares the bearer token in constant time.

This is a *trigger*, not an agent surface: a webhook can start a pipeline, but the pipeline still runs its own approval gate, so an inbound request can never make the LLM fire an outbound action on its own.

## Configuration

```yaml
provider:
  type: openrouter
  api_key_env: OPENROUTER_API_KEY

models:
  haiku: {model: anthropic/claude-haiku-4-5, max_tokens: 1024}

budgets:
  per_step_tokens:     2048
  per_pipeline_tokens: 10000
  per_day_tokens:      100000

state:
  path: ./state.db

observability:
  spans: false           # structured JSON spans per pipeline/step (or DRAFTCAT_TRACE=1)
```

Skills are YAML prompt templates in `skills/` with an `output_schema` the engine enforces. With `-tags voice`, an additional `voice:` block configures the webhook receivers, Dograh endpoints, and pre-call lookup — full reference in [docs/voice.md](docs/voice.md).

## Commands

```bash
draftcat                       # run the engine
draftcat validate [--strict]   # lint config + skills
draftcat test <pipeline>       # dry-run against fixtures/<pipeline>/ (never touches real APIs)
```

Pre-commit hooks (lefthook) run `gofmt`, `go vet`, `go build`, and `go test -short`; pre-push runs `draftcat validate --strict`.

## Voice AI plugin

Built with `-tags voice`, Draftcat becomes the **EU-resident writeback + governance layer** for self-hosted voice agents (Dograh, Pipecat, or any orchestrator that posts JSON webhooks) — AI calling, inbound qualification, and support deflection in DE / AT / CH with audio + transcripts never leaving the chosen EU region.

- **5 lifecycle webhook receivers** (`session_start`, `event`, `session_end`, `handoff`, `learning`)
- **Pre-call context lookup** (sub-300ms p95) enriches the greeting from GHL / custom CRM before the agent speaks
- **7-step Learning-Item review pipeline** before any prompt / workflow / KB change reaches production
- **Dograh admin actions** drive its REST API directly (git-commit workflow, staging smoke, prod publish)
- **Per-day call + minute budgets** and **bearer-token webhook auth** (constant-time compare)

The 5-endpoint writeback contract is orchestrator-agnostic. Full wiring recipe and runnable [DACH fixtures](fixtures/voice-dach-screener/pipeline.yaml) in [docs/voice.md](docs/voice.md).

## Patterns explained

The deterministic-boundary architecture is documented in the **Production AI Automation Notes** gist series, each mapping to draftcat code:

- [#1 Agent Approval Gates](https://gist.github.com/renezander030/9069db775e494ffd2cdd5a09adf83add) — proposed actions, schema validation, audit log
- [#2 Token Budgets](https://gist.github.com/renezander030/a7d99ad94b97f7943a9a04016d62faaa) — per-step / pipeline / day enforcement (`main.go`)
- [#5 SQLite Dedup + Crash Safety](https://gist.github.com/renezander030/8a23e32cde0c882a5aa069c4bfdf697f) — WAL mode, `seen_items`, run audit (`internal/state/`)
- [#6 Prompt-Injection Defense](https://gist.github.com/renezander030/213ffdf1ab1bdb169881927bc7080270) — input sanitization + output schema validation
- [#7 PDF Cite Verification](https://gist.github.com/renezander030/7780cbc0b3ad4e802e8fba8bfc1c3a66) — auditable LLM extraction with per-fragment bounding boxes (`internal/pdf/`)

## Related projects

- [capcut-cli](https://github.com/renezander030/capcut-cli) — edit CapCut / JianYing video drafts from the CLI. Same DNA: single binary, no API, structured JSON boundary between agent and tool.

## Status

**v0.2** — early access. Single-business, single-operator deployments. Public APIs may change between minor versions until v1.0.

New in v0.2: webhook triggers (`schedule: webhook`), structured observability spans, and `enum` / `number` enforcement in output schemas.

Planned: a generic HTTP action, per-step retry + circuit breaker, Slack approval channel, and an OpenTelemetry/Prometheus span exporter. Voice v0.2: outbound campaigns with consent gating, recording redaction, and an MCP bridge. See [docs/voice.md](docs/voice.md) for the voice roadmap.

## License

MIT. See [LICENSE](LICENSE).
