# Budgets and tool execution lifecycle

Draftcat keeps model usage in SQLite, isolates each pipeline run's token and cost totals, and records tool authorization separately from external execution. These controls use the existing service and CLI.

## Daily and per-run budgets

Daily usage is keyed by the full UTC calendar date and survives engine restarts. Model calls enter one context-aware admission gate, check settled usage, and write a pending usage row before contacting the provider. Separate instances sharing the same database cannot admit a second model call while one has unsettled usage. Each pipeline run has its own `per_pipeline_tokens` and `per_pipeline_cost` counters.

Every engine model request passes this boundary, including intent classification and operator-requested rewrites. Responses are charged before model output policy is applied, so a paid response remains counted when its content is denied or sent for human review. Human output review does not hold the provider admission gate.

`per_step_tokens` bounds the requested completion length together with the selected model's `max_tokens`. Daily and pipeline token totals count both input and output usage reported by the provider. Token limits and money caps stop further calls based on settled usage; an admitted call can cross a threshold because the final prompt usage and charge are only known afterward. A money cap is a stop threshold rather than a prepaid dollar reservation. Configure small completion limits to bound generation work.

Provider response bodies have a 4 MiB limit. Unreadable, oversized, malformed, or invalid-usage responses stop processing. Rate-limit responses can retry within the configured retry count and request deadline. Transport errors and responses with uncertain billing do not automatically retry as another paid request.

## Inspect and reconcile uncertain usage

A crash during a provider call, or a response without trustworthy usage, leaves a pending usage row. Subsequent model calls remain blocked until the operator verifies the actual usage. This applies even when the pending call began on an earlier UTC date.

```bash
draftcat budget status --json
```

Stop the engine before reconciliation. Check the provider's usage record, then charge the actual total tokens and cost for the listed call:

```bash
draftcat budget reconcile <call-id> --tokens 1200 --cost 0.0042
```

The charge belongs to the original call's UTC date. Reconciliation settles the call once; it cannot charge a completed call again. Use zero only when the provider confirms that no tokens or money were billed. Both commands accept `--config path`, and `DRAFTCAT_STATE_PATH` takes precedence. Inspection is read-only and a misspelled state path does not create a new database. Start the engine once to migrate an older database before inspecting the new budget tables.

## Approval storage

Pipeline approvals, automatic approval policies, and model policy reviews require their durable records to succeed before they release work. Tool decisions and permit consumption commit their receipt and state transition in one transaction. An unavailable database or failed audit write blocks release.

Existing receipt versions and signatures remain valid. New tool consumption receipts retain their existing action, payload, policy, and expiration binding. This release adds budget and outcome tables without replacing historical receipts.

## Revoke an unconsumed tool action

Use the same authenticated tool-gate listener and the exact `binding_hash` returned with the action:

```http
POST /gate/tool-call/<action-id>/revoke
Authorization: Bearer <webhook-secret>
Content-Type: application/json

{"binding_hash":"sha256:<binding-digest>"}
```

A pending or allowed action becomes `revoked`. A matching retry returns the same state. Revocation and consumption race through conditional database transitions: only one can win. A late human approval cannot restore a revoked action. Revocation cannot interrupt an external action whose permit was already consumed; that request receives HTTP 409.

The service's bearer credential authorizes this route. Keep it in the trusted harness or operator application. When `webhook.require_signature` is enabled, lifecycle POST requests also need the existing signed-body header described in the [tool gate guide](tool-gate.md).

## Record an external execution outcome

After the harness successfully consumes a permit and executes its bound action, it can attest to the result:

```http
POST /gate/tool-call/<action-id>/complete
Authorization: Bearer <webhook-secret>
Content-Type: application/json

{"binding_hash":"sha256:<binding-digest>","status":"succeeded","result_hash":"sha256:<result-digest>"}
```

`status` is `succeeded` or `failed`; `result_hash` is optional and must be a SHA-256 digest when supplied. Send a hash of the result rather than the result itself. The record is durable, immutable, and bound to the consumed action. Matching retries succeed; a different status or hash receives HTTP 409. Reporting an outcome never grants another execution permit.

Authenticated polling includes `execution_status`: `not_started`, `unreported`, `succeeded`, or `failed`. Reported outcomes also include `completed_at`, optional `result_hash`, and `execution_evidence: caller_attestation`. This records the harness's assertion; Draftcat does not independently observe or prove the external side effect. A missing acknowledgment remains `unreported`, preserving the uncertainty after a harness crash.

Live and recovered permit status use the same expiration and current policy checks. A permit expires at its recorded expiration instant. A policy change invalidates unconsumed authorization and requires a new action approval.

## Exact structured output

The existing flat `output_schema` supports `int`, `number`, `bool`, and `string` types, numeric `min`/`max`, and scalar `enum` values. Every declared field is required. Additional model output fields remain accepted for compatibility.

Integer fields require mathematical integers, so `1.0` and `1e3` are valid while `1.5` is rejected. JSON numeric tokens and YAML schema numeric values are compared without a float64 round trip. Duplicate keys, trailing content, non-object output, unsupported constraints, malformed definitions, and non-scalar enum values are rejected. Startup validation checks schemas from both prompt skills and inline pipeline steps. This is Draftcat's flat schema contract; full JSON Schema keywords are not supported.
