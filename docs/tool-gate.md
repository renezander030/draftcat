# Tool-call gate

`POST /gate/tool-call` puts one tool call an agent is about to make through the
same gate a pipeline step goes through: allowlist, risk tier, human approval,
audit row. It is served on the webhook listener, so `tool_gate.enabled` requires
`webhook.enabled`. Every ask, poll, and consume request requires the webhook
bearer token. If `webhook.require_signature` is enabled, gate POST bodies also
require `X-Draftcat-Signature`.

Two properties matter more than convenience, and everything below preserves
them:

- **Default deny.** A tool nobody listed is refused. Forgetting to configure a
  tool fails closed, not open.
- **The arguments are part of the approval.** The gate hashes the exact
  arguments proposed and the human approves *those*. A harness that then calls
  the tool with different arguments leaves an audit trail that does not match.
- **One decision releases one execution.** An allowed decision is not itself an
  execution permit. The caller atomically consumes its exact binding, and only
  that first response carries `"permit":"execute"`.

## Asking

```
POST /gate/tool-call
Content-Type: application/json
Authorization: Bearer <DRAFTCAT_WEBHOOK_SECRET>

{
	"action_id": "send-invoice-2026-114",
  "tool":   "send_email",
  "args":   {"to": "anna@example.com", "subject": "Invoice 2026-114"},
  "agent":  "harness-1",
  "run_id": "01J8Z6..."
}
```

| Field       | Meaning |
|-------------|---------|
| `action_id` | Stable caller identity for this intended side effect. Matching retries return the existing state; changed data under the same ID gets `409`. Omitted clients receive a generated ID, but cannot safely retry across a restart without persisting it. |
| `tool`   | Required. Looked up in `tool_gate.tools`. |
| `args`   | The exact arguments the harness intends to call with. Hashed into the decision, never stored. |
| `agent`  | Free-form caller name, shown to the operator and used to key the repeat guard. |
| `run_id` | Optional. Ties the decision to a pipeline run in the audit trail. |
| `mode`   | `sync` (default) or `async` — see *Collecting a decision*. |
| `wait`   | A duration such as `30s`. Bounds a sync hold; see below. |
| `expires_at` | Optional RFC3339 deadline that can narrow, never extend, the approval window. |

Every answer has the same shape:

```json
{"action_id":"send-invoice-2026-114","decision":"allow","state":"allowed",
 "args_hash":"sha256:9f2b...","policy_hash":"sha256:41a0...",
 "binding_hash":"sha256:acd1...","decided_by":"operator",
 "consume":"/gate/tool-call/send-invoice-2026-114/consume","expires_at":"2026-09-17T18:00:00Z"}
```

`decision` is `allow`, `deny` or `pending`. `decided_by` says who settled it:
`allowlist` (the rule itself), `policy` (an argument rule), `operator` (a
human), or `repeat-guard`. `rule` names the config that produced a decision the
gate made on its own, so a denial can be traced without opening the audit log.
`state` is `pending`, `allowed`, `denied`, `expired`, or `consumed`.

## What the rule decides

```yaml
tool_gate:
  enabled: true
  tools:
    - name: read_calendar            # listed, no approval: allowed, audited as policy_approve
    - name: send_email
      risk: high
      require_approval: true         # every call reaches a human
    - name: create_invoice
      args:                          # arguments inside the rule: allowed silently
        customer: {one_of: [acme, globex]}
        amount:   {max: 500}
      on_mismatch: approve           # outside it: ask a human (default)
    - name: post_to_slack
      args:
        channel: {glob: "#ops-*"}
      on_mismatch: deny              # outside it: refuse without asking
```

The order of checks, for a listed tool:

1. **Arguments** are checked against `args:`. Each key names a top-level
   argument. A call matches when every constraint holds; a listed key that is
   absent is a mismatch unless the constraint says `optional: true`.
2. On a **match**, the rule's base setting applies: `require_approval: true`
   asks a human, otherwise the call is allowed on the strength of being listed.
3. On a **mismatch**, `on_mismatch` applies: `approve` (default) sends the call
   to a human even if the rule would otherwise allow it; `deny` refuses without
   asking. A mismatch can only tighten a rule. It never lets a call through
   that a match would have sent to a human.

Constraints, all of which must hold when several are given on one argument:

| Key        | Holds when |
|------------|------------|
| `equals`   | the value, in string form, equals this (numbers compare by value) |
| `one_of`   | the value is one of the listed strings |
| `glob`     | the value matches a `path.Match` pattern (`*`, `?`, `[...]`) |
| `regex`    | the value matches a Go regular expression — write `^` and `$` to anchor it |
| `min`/`max`| the value is a number inside the bound |
| `optional` | *(modifier)* the argument may be absent |

Values are compared in their string form: strings as-is, numbers without an
exponent, booleans as `true`/`false`, anything structured as compact JSON.
`draftcat validate` refuses a regex that does not compile, a malformed glob, a
`min` above its `max`, and a constraint with no condition (almost always a
typo'd key).

## Collecting a decision

A decision the rule settles comes back at once. A decision that needs a human
takes as long as the human takes — up to `timeouts.operator_approval`, 4h by
default — and an HTTP client that gives up after 30 seconds used to lose it.
Three ways to hold on:

**Sync (default).** The request is held until the operator decides or the
window closes. The response carries `approval_id` either way. If the harness
hangs up first, the gate treats that as a withdrawn request: the prompt is
closed as timed out and the call is denied.

**Bounded hold.** `"wait": "30s"` holds for at most 30 seconds. If the human has
not decided by then the gate answers `202` with `decision: pending`, the
`approval_id`, a `poll` path and `expires_at`, and the approval keeps running
server-side.

**Async.** `"mode": "async"` answers `202 pending` immediately.

Either way the harness collects the decision from (using the bearer token):

```
GET /gate/tool-call/<approval_id>            # 200 pending | allow | deny
GET /gate/tool-call/<approval_id>?wait=30s   # long-poll, capped at 100s
```

An unknown id answers `404` with `decision: deny`. Durable action state remains
queryable after restart. Work that was still pending when the process stopped
becomes `expired`; it is never resumed from an unprovable in-memory point.

## Consuming an allowed decision

Do not execute on `state: allowed`. Atomically consume the exact binding first:

```
POST /gate/tool-call/send-invoice-2026-114/consume
Authorization: Bearer <DRAFTCAT_WEBHOOK_SECRET>
Content-Type: application/json

{"binding_hash":"sha256:acd1..."}
```

The first matching request before expiry returns `200` with
`"state":"consumed","permit":"execute"`. That response authorizes exactly one
execution of the bound action. A second consume, the wrong binding, a denial,
or an expired permit returns `409` without `permit: execute`.

The action ledger is in SQLite. A retry using the same `action_id`, tool,
arguments, caller, run, policy, and expiry returns its current state without a
second prompt. Reusing the ID after any of those bound facts changes is a `409`
conflict and requires a new action ID and a fresh decision.

A tool call waiting on a human is written to `pending_approvals` before the
prompt goes out, exactly like a pipeline gate. It shows up in `/pending` and
`draftcat pending` as `tool-gate / <tool>`, and a process that dies mid-wait
reconciles it at next boot as `interrupted` — audit row included — instead of
the gate vanishing without a trace.

## The repeat guard

An agent that loops on one call does not get a fresh prompt each time. Inside
`tool_gate.repeat_window` (default `10m`, `0` disables) the gate remembers its
decision on an identical call — same `agent`, same `tool`, same argument hash:

- an identical call **still being decided** joins the open prompt and receives
  the same decision (and the same `approval_id`), so a harness that retries a
  timed-out request does not page the operator twice;
- a call the operator or a rule **denied** is denied again without asking,
  `decided_by: repeat-guard`, `rule: repeat_window`;
- a call the operator **approved** is asked again by default — approving one
  send does not approve the next, because the next one has the same side
  effect. A rule may opt in with `remember_approval: true` for tools whose
  repeat is harmless (a read, a lookup). `draftcat validate` refuses that on a
  high-risk tool.
- `tool_gate.max_repeats: N` caps how many prompts one identical call may raise
  inside the window; beyond it the gate denies with `rule: max_repeats`.

Changed arguments are a different call and start fresh.

## Telling the operator

A refusal the gate makes on its own — an unlisted tool, an argument outside a
`deny` rule, the repeat guard — used to exist only in the log. Now the operator
channel hears about it:

```
[tool-gate] DENIED send_email (agent: harness-1)
reason: tool "send_email" is not in tool_gate.tools — the gate denies by default
args sha256: sha256:9f2b...
```

One notice per agent, tool and reason inside `tool_gate.notify_window`
(default `10m`), so a looping agent produces one line rather than a page of
them. A human's own Skip is not repeated back. `notify_denials: false` turns
the notices off.

## Audit

Every decision writes a v2 `action_approvals` receipt with `pipeline = tool-gate`,
`step = <tool>`, the argument hash as the payload hash, and the reason or rule
in the `policy` column. The signed envelope also carries the stable action ID,
policy digest, exact binding, run ID, and expiry. `draftcat receipts list`,
`show`, and `export` read the rows directly from SQLite.

## Configuration reference

```yaml
tool_gate:
  enabled: true              # requires webhook.enabled; served on the same listener
  repeat_window: 10m         # remember a decision on an identical call; 0 = off
  max_repeats: 0             # prompts allowed per identical call inside the window; 0 = no cap
  notify_denials: true       # operator notices for denials the gate made on its own
  notify_window: 10m         # dedup window for those notices
  tools:
    - name: <tool>
      risk: low | normal | high      # default normal; high always needs a human
      require_approval: true|false   # ask a human on every (matching) call
      remember_approval: true|false  # reuse an approval inside repeat_window (never for high risk)
      args:
        <argument>: {equals: …, one_of: […], glob: …, regex: …, min: …, max: …, optional: true}
      on_mismatch: approve | deny    # default approve
```
