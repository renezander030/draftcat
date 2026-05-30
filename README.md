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

## Why Draftcat

|                              | **Draftcat**                                   | **n8n**                                   | **LangChain agents**             |
| ---------------------------- | ---------------------------------------------- | ----------------------------------------- | -------------------------------- |
| **Built for**                | AI-augmented business ops with governance      | General workflow automation               | Open-ended agent loops           |
| **AI execution model**       | Deterministic boundary; AI cannot fire actions | Bolt-on LLM nodes in visual workflows     | Agent decides next action freely |
| **Human-in-the-loop**        | Required on every outbound step                | Optional manual nodes                     | Optional; not the default        |
| **Token budgets**            | Per-step / pipeline / day, enforced            | None                                      | None                             |
| **Prompt-injection defense** | Input sanitization + output schema validation  | None                                      | None                             |
| **State & dedup**            | SQLite-backed; items processed at most once    | DB-backed                                 | In-memory                        |
| **Runtime**                  | Single Go binary                               | Node.js + Postgres                        | Python + dependency tree         |
| **Voice AI**                 | Built-in `voice` plugin (Dograh, EU residency) | None                                      | None                             |

Use n8n for drag-drop integrations across 400+ services. Use LangChain for research and open-ended exploration. Use Draftcat when a wrong LLM choice means a real customer gets emailed.

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

Add an action by appending a `case` to the deterministic switch in `main.go` and registering its name in `validate.go`. See `internal/ghl/ghl.go` for the connector pattern, `voice_actions.go` / `dograh_admin.go` for build-tag-gated examples.

## Governance

- **Token budgets** — per-step / per-pipeline / per-day caps. Exceeding any budget halts the pipeline immediately.
- **Human-in-the-loop** — every outbound action requires explicit operator approval.
- **Input sanitization** — operator input is scrubbed for prompt-injection patterns before reaching the LLM.
- **Output validation** — AI output is validated against the skill's JSON schema; invalid output is rejected.
- **Rate limiting** — per-user, per-minute limits on operator interactions.
- **Channel security** — allowed-user lists and input-length limits enforced at startup; the engine refuses to start without them.

## State & idempotency

State persists to SQLite at the path set in `config.yaml` (`./state.db` by default).

- **Dedup** — fetched item IDs are recorded per `(pipeline, scope)`; subsequent runs skip already-processed items. Marked seen at fetch time, so a downstream failure won't cause reprocessing. Replay via the `/run <pipeline>` operator command.
- **Run history** — every run records `pipeline / started_at / ended_at / status / error_text`.
- **Crash safety** — WAL mode + `synchronous=NORMAL` for durable writes without per-write fsync.

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

**v0.1** — early access. Single-business, single-operator deployments. Public APIs may change between minor versions until v1.0.

Planned: webhook triggers, a generic HTTP action, per-step retry + circuit breaker, Slack approval channel, and Prometheus metrics. Voice v0.2: outbound campaigns with consent gating, recording redaction, and an MCP bridge. See [docs/voice.md](docs/voice.md) for the voice roadmap.

## License

MIT. See [LICENSE](LICENSE).
