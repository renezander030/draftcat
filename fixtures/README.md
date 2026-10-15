# Fixtures

`draftcat test <pipeline>` reads files from `fixtures/<pipeline>/` to dry-run a pipeline without touching real connectors, the LLM, or the operator channel.

## Layout

```
fixtures/<pipeline-name>/
  _input.json         (optional) seeded into the pipeline data map before any step
  <step-name>.json    one file per step that needs input
  _expect.json        (optional) what the dry run must end with
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

## Expectations: `_expect.json`

With `_expect.json` in the fixture directory, `draftcat test` checks the end of
the dry run and exits 1 when anything differs, so a prompt, skill or schema
change that alters the outcome fails in CI:

```json
{
  "outcome": "completed",
  "data": {
    "ai_output.score": 4,
    "ai_output.reject": false,
    "approved": true
  },
  "absent": ["error"],
  "drafts_contain": ["Strong fit"],
  "sent_contain": []
}
```

| Key | Holds when |
|---|---|
| `outcome` | `completed` (the last step ran) or `skipped` (an operator skip ended the pipeline) |
| `stopped_at` | a skip stopped the pipeline at this approval step |
| `data` | each dotted path into the final data map holds this value, compared as JSON |
| `absent` | none of these paths is set |
| `drafts_contain` | each text appears in some approval draft |
| `sent_contain` | each text appears in some operator message |

Every key is optional. An unknown key is refused, so a typo never asserts
nothing. Unmet expectations are listed one per line:

```
[expect] FAIL data.ai_output.score: 3, expected 4
[test] FAILED: 1 expectation(s) not met (fixtures/test-screener/_expect.json)
```
