# Fixtures

`draftcat test <pipeline>` reads files from `fixtures/<pipeline>/` to dry-run a pipeline without touching real connectors, the LLM, or the operator channel.

## Layout

```
fixtures/<pipeline-name>/
  _input.json         (optional) seeded into the pipeline data map before any step
  <step-name>.json    one file per step that needs input
```

## Per-step shape

| Step type       | Fixture shape                                                                | Behavior if missing                          |
| --------------- | ---------------------------------------------------------------------------- | -------------------------------------------- |
| `deterministic` | `{"data": {key: value, ...}}` or a flat map; merged into the pipeline data   | Step is a no-op                              |
| `ai`            | `{"text": "<verbatim model response>"}` — validated against the skill schema | **Error** — supply the response              |
| `approval`      | `{"action": "approve\|skip\|adjust", "text": "<optional adjust feedback>"}`  | Auto-approve (or skip with `--reject`)       |

## Example: `test-screener`

The `test-screener` pipeline in `config.yaml` has steps `mock-input`, `classify` (skill `classify-job`), `review` (approval), `log-result`. The fixtures here drive each step:

- `mock-input.json` seeds `data.input` with a job posting.
- `classify.json` supplies the model's JSON response.
- `review.json` auto-approves.

Run:

```bash
draftcat test test-screener
```
