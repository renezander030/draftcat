# The `http_request` action

`http_request` sends one HTTP request from a pipeline, after an operator
approved it. Draftcat holds the credential (read from an environment variable
you name) and sends the request itself, so the model that drafted the payload
never sees the key and cannot call the endpoint on its own. It covers any API
that takes a JSON or text request, without a connector written for it.

## Example

An AI step drafts an order, the operator reviews the request it will make, and
draftcat posts it with a key the agent never holds:

```yaml
pipelines:
  - name: place-order
    schedule: webhook
    steps:
      - name: draft
        type: ai
        role: drafter
        prompt: |
          Turn this request into an order. Reply with JSON only.
          {{webhook_body}}
        output_schema:
          sku:      {type: string}
          quantity: {type: int, min: 1}
          note:     {type: string}
      - name: review
        type: approval
        channel: telegram
      - name: submit
        type: deterministic
        action: http_request
        vars:
          url: https://api.example.com/v1/orders
          auth_env: ORDERS_API_KEY        # Authorization: Bearer <value of ORDERS_API_KEY>
          headers: |
            X-Source: draftcat
```

The operator sees the request itself, not the model's draft:

```
[draftcat] Outbound request for review:

POST https://api.example.com/v1/orders
Content-Type: application/json
X-Source: draftcat
Authorization: Bearer <credential from ORDERS_API_KEY>
Idempotency-Key: 4c1f…

{
  "note": "…",
  "quantity": 2,
  "sku": "AAA-1"
}
```

## Vars

| Var | Default | Meaning |
|---|---|---|
| `url` | required | Absolute `http` or `https` URL. Not templated. A URL with `user:password@` is refused. |
| `method` | `POST` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE`. |
| `body_key` | `ai_output` for POST, PUT and PATCH | The run data value sent as the body. A map or list is sent as indented JSON; a string that holds a JSON object or array is sent as that document; other text is sent as a JSON string, or as is when the body is not JSON. |
| `body` | none | A template instead of `body_key`. `{{key}}` is replaced by that run data value, encoded as a JSON value when the body is JSON (`{"text": {{ai_raw}}}` quotes the text). A missing key, or a JSON body that does not parse once filled, stops the step. |
| `headers` | none | Static, non-secret headers, one `Name: value` per line. `Authorization`, `Idempotency-Key` and the credential header are refused here. A `Content-Type` line sets the body's type (default `application/json`). |
| `auth_env` | none | Environment variable holding the credential. Empty at send time = the step fails and nothing is sent. |
| `auth_header` | `Authorization` | Header that carries the credential. |
| `auth_scheme` | `Bearer` when the header is `Authorization`, else none | Word placed before the credential (`Token`, `Basic`). A custom `auth_header` carries the bare credential unless a scheme is set. |
| `timeout` | `30s` | Limit for the whole request, within the pipeline's own limit. |
| `expect_status` | `2xx` | Statuses that count as success: codes and classes, comma separated (`200,201`, `2xx,304`). |
| `max_response_bytes` | `1048576` | Most of the response body kept. A longer body is cut, kept as text and marked `truncated`. |

GET and DELETE send no body unless `body` or `body_key` is set; GET never sends one.

## What the approval binds

The approval step before an `http_request` shows the operator the request the
step will send: method, URL, every header (the credential named, never shown),
the idempotency key and the body. Its approval is bound to a digest of exactly
that request, in this run. The `http_request` step renders the request again
and sends it only when the digest matches:

- No approval step showed it in this run: the step fails, nothing is sent.
- The run's data changed between approval and send, so the request differs
  from the one approved: the step fails, nothing is sent. Put the approval step
  directly before the request.
- The operator skips: the run stops there, as with any approval.
- The operator asks for a change: the model rewrites its output, the rewrite
  becomes the output the request is built from, and the operator reviews the
  new request. A rewrite is not checked against the AI step's `output_schema`;
  the operator sees the request it makes before approving it.

An approval step released by `approval_policy` (a `risk: low` step under an
`auto_approve` rule) releases the request too, recorded as `policy_approve` in
the audit trail. That is the only way to send without a person tapping approve,
and a `risk: high` step can never take it.

One approval step releases one request: the first `http_request` after it. A
second request needs its own approval step.

## Sending

- Each request carries `Idempotency-Key`, derived from the run id and the step
  name, so a receiver that supports it can drop a repeat.
- There is no retry. A timeout or a dropped connection fails the step, and the
  error says the receiver may or may not have acted on it.
- Redirects are not followed, so the credential never reaches a host the
  configuration did not name. A 3xx fails the step unless `expect_status`
  allows it.
- A status outside `expect_status` fails the step with the status and the
  first 300 bytes of the response.

## Response in the run data

Later steps read the response from the run data:

| Key | Value |
|---|---|
| `http_response` | `status`, `headers` (Content-Type, Location, ETag, Last-Modified, Retry-After, Request-Id, X-Request-Id), `body` (parsed when it is complete JSON, else text), `truncated`, `idempotency_key` |
| `http_status` | The status code as text, for prompts (`{{http_status}}`) |
| `http_response_body` | The response body as text |

A second `http_request` in the same run replaces these keys.

## The credential

The value of `auth_env` is read when the step sends, never written to config,
state or the run data, and joins the redaction set like every other configured
credential: it is replaced by `[REDACTED:<VAR>]` in the log, operator messages,
stored run errors and exported spans. Failure messages and the response text
are scrubbed of it as well.

## Validation

`draftcat validate` and the startup check report, for each `http_request` step:

- errors: `url` missing or not `http(s)`; no approval step before the step
  (after any earlier `http_request`); a credential header that cannot be
  formed from `auth_header` / `auth_scheme`; a bad `method`, `timeout`,
  `expect_status` or `max_response_bytes`; both `body` and `body_key`; a body
  on GET; a credential or `Idempotency-Key` in `headers`.
- warnings: the `auth_env` variable is empty; plain `http://` to a host other
  than loopback; `auth_scheme` or `auth_header` without `auth_env`; a var the
  action does not read.
