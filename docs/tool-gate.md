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
| `max_per_day` | the value is a number of 0 or more, and today's total stays inside the cap (below) |
| `optional` | *(modifier)* the argument may be absent |

`review_above` is not a condition but a threshold: a value above it goes to a
human even when the rule would allow the call silently (below).

Values are compared in their string form: strings as-is, numbers without an
exponent, booleans as `true`/`false`, anything structured as compact JSON.
`draftcat validate` refuses a regex that does not compile, a malformed glob, a
`min` above its `max`, a `review_above` at or above `max`, a negative
`max_per_day`, and a constraint with no condition (almost always a typo'd key).

## Amount bands and daily totals

Money-shaped arguments usually need three bands and a running total:

```yaml
tool_gate:
  tools:
    - name: create_invoice
      on_mismatch: deny
      args:
        amount:
          min: 0
          review_above: 500     # up to 500: allowed; above: a human decides
          max: 5000             # above 5000: refused (on_mismatch: deny)
          max_per_day: 10000    # today's allowed total may not pass 10000
```

- **`review_above`** sends a call whose value is above it to a human, with
  `rule: above the review threshold — amount: 800 is above review_above 500`
  on the prompt. Calls at or below it keep the rule's own setting.
- **`max_per_day`** keeps a running total of the argument across the tool's
  allowed calls in the current UTC day. A call that would take the total past
  the cap is handled like an argument mismatch: `on_mismatch: deny` refuses it
  (`rule: max_per_day.amount`), `approve` asks a human. Every prompt for a
  capped tool shows the total so far, for example
  `today: amount: 1200 allowed today, max_per_day 10000`, and a human approval
  counts toward the total like any other.

A call counts from the moment it is allowed; a permit that is revoked or
expires unconsumed stops counting. Totals live in the state store, so they hold
across restarts, and capped decisions are made one at a time so two concurrent
calls cannot both fit under a total that only one fits under. The argument
values under a cap are stored next to the action; all other arguments remain
hashed only. When the total cannot be read, the gate denies.

### Calls per day

`max_calls_per_day` on a rule caps how many calls of the tool the gate allows
in one UTC day, whatever the arguments:

```yaml
    - name: send_email
      on_mismatch: deny
      max_calls_per_day: 200   # the 201st call today is refused
```

Calls count like a `max_per_day` total: from the moment one is allowed, by a
rule or by a human, until its permit is revoked or expires unconsumed. A call
past the cap follows `on_mismatch` (`rule: max_calls_per_day`), and prompts for
the tool show `today: calls: 37 allowed today, max_calls_per_day 200`.

Adding `review_above`, `max_per_day` or `max_calls_per_day` changes that rule's
policy hash, as any rule edit does; rules without them keep their hash.

## Checking a policy offline

`draftcat gate` decides calls against `tool_gate` with the same evaluation the
endpoint uses, without starting the engine or touching the state store:

```bash
draftcat gate check create_invoice --args '{"customer": "acme", "amount": 800}'
draftcat gate check create_invoice --args '{"amount": 300}' --spent amount=9800 --json
```

```
ASK  create_invoice
  reason: a human decides
  why:    above the review threshold — amount: 800 is above review_above 500
  today:  amount: 0 allowed today, max_per_day 10000
```

The decision is `allow`, `deny`, or `ask` (a human decides). `--spent` sets an
argument's total already allowed today, and `--calls` the tool's calls already
allowed today (`calls:` in a test case). The repeat guard and the operator
channel are runtime state and are not part of the check.

`draftcat gate test` runs a file of cases and exits 1 when a decision differs,
so a policy change can be reviewed in CI like code:

```yaml
cases:
  - name: small invoice passes
    tool: create_invoice
    args: {customer: acme, amount: 200}
    expect: allow
  - name: split invoices stop at the daily cap
    tool: create_invoice
    args: {amount: 300}
    spent: {amount: "9800"}
    expect: deny
    rule: max_per_day.amount      # optional: also pin the deciding rule
  - name: unknown tools are refused
    tool: wire_money
    expect: deny
```

```
PASS  small invoice passes: ALLOW  create_invoice
...
3 case(s), 0 failed
```

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

### Target preconditions

When a call changes a record, the harness can name the state of the target it
read, for example its version, ETag or content digest:

```json
{"action_id": "crm-update-88", "tool": "update_record",
 "args": {"id": 88, "stage": "won"}, "precondition": "etag:\"v7\""}
```

The prompt shows `target: etag:"v7"`, and the precondition is bound into the
permit. To consume it, the harness reads the target again and sends what it
found:

```json
{"binding_hash": "sha256:...", "precondition": "etag:\"v7\""}
```

The same value consumes the permit. A different or missing value answers `409`
with `rule: precondition_failed`, no `permit: execute`, and the permit stays
unconsumed; a target that changed needs a new approval. A precondition is at
most 512 printable characters. Calls without one bind and consume as before.

### Execution grants

With a grant key configured, every successful consume also returns an
`execution_grant`, a signed statement that this permit was consumed for this
exact call:

```yaml
tool_gate:
  execution_grant:
    secret_env: DRAFTCAT_GRANT_SECRET   # the tool holds the same key
```

The tool checks the grant before it acts, so a call that never went through
the gate has no grant the tool accepts:

```bash
draftcat gate verify-grant "$GRANT" --tool send_email \
  --args '{"to":"billing@example.com"}' --max-age 5m     # exit 0 valid, 1 not
```

A grant is `dcg1.<payload>.<mac>`: the payload is base64url JSON with
`action_id`, `tool`, `agent`, `run_id`, `args_hash`, `binding_hash`,
`consumed_at` and `expires_at`; the mac is base64url HMAC-SHA256 over
`dcg1.<payload>` with the key, so a tool in any language can check it. The
`args_hash` is `sha256:` over the arguments as compact JSON with sorted keys.
A grant covers one execution: a tool that keeps the `action_id`s it has run
refuses a second use.

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

With `notify_outcomes: failed` (or `all`), the operator also hears what an
allowed action did once the harness reports its outcome through
`POST /gate/tool-call/<id>/complete`:

```
[tool-gate] send_email FAILED (agent: harness-1)
action: inv-2026-0412
result sha256: sha256:4f1c...
```

One notice per recorded outcome; a retried report does not repeat it. The
default is `off`.

## Standing grants

A rule with `allow_grants: true` lets an operator pre-approve a bounded run of
its calls from the Telegram operator chat:

```
/grant send_email 2h 20           # the next 20 calls within 2 hours
/grant send_email 30m 5 inbox     # only for agent "inbox"
/grants                           # live grants: uses left, time left, who issued them
/ungrant g_3f9a1c22e0b4           # end one now
```

While a grant lasts, a call the rule would ask about only because it has
`require_approval` is allowed without a prompt, with `decided_by: grant` and
`rule: grant:<id>`. Its receipt is a `policy_approve` that names the grant, the
operator who issued it and the use it took (`standing grant g_3f9a1c22e0b4 by
operator 111111, use 3 of 20`). The earliest-expiring grant that covers the
call is used, and each use is taken once even under concurrent calls.

A grant does not cover:

- a call with arguments outside the rule, or past a `max_per_day` total or
  `max_calls_per_day` — those still reach a human (`review_above` never
  applies on a rule with `require_approval`, which already asks every time);
- a high-risk tool — `draftcat validate` refuses `allow_grants` on one;
- a rule that no longer has `allow_grants` — its grants stop applying.

A grant lasts between 1 minute and 24 hours and covers at most 1000 calls
(1000 when no count is given). Grants live in the state store and survive a
restart.

## Caller tokens

Each harness can hold its own token instead of the webhook token:

```yaml
tool_gate:
  require_caller: true            # optional: refuse the webhook token on the gate
  callers:
    - name: billing-agent
      token_env: GATE_TOKEN_BILLING
      tools: [create_invoice]     # optional: only these tools
    - name: inbox-agent
      token_env: GATE_TOKEN_INBOX
```

A request with a caller's token is made as that caller:

- its `agent` is the caller's name, recorded on the action and bound into the
  permit; a request that names a different agent is refused with `403`;
- a tool outside its `tools` is denied with `rule: caller.tools`;
- `GET`, `consume`, `revoke` and `complete` reach only the actions it asked
  for, and any other id answers `404`;
- a signed request is signed with the caller's own token.

Removing a caller, or unsetting its variable, withdraws that one token. The
webhook token keeps working on the gate for every listed tool unless
`require_caller: true`. Caller tokens are redacted from logs and notices like
every other credential. `draftcat validate` refuses duplicate names and shared
variables, and `draftcat doctor` warns about a caller whose variable is empty.

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
  notify_outcomes: off       # off | failed | all: tell the operator what allowed actions did
  require_caller: false      # refuse the webhook token; only caller tokens reach the gate
  execution_grant:
    secret_env: <VARIABLE>   # sign an execution grant on every consume
  callers:
    - name: <agent>          # the agent name the token is recorded under
      token_env: <VARIABLE>  # env var holding the caller's bearer token
      tools: [<tool>, …]     # optional: the tools this caller may ask about
  tools:
    - name: <tool>
      risk: low | normal | high      # default normal; high always needs a human
      require_approval: true|false   # ask a human on every (matching) call
      remember_approval: true|false  # reuse an approval inside repeat_window (never for high risk)
      args:
        <argument>: {equals: …, one_of: […], glob: …, regex: …, min: …, max: …,
                     review_above: …, max_per_day: …, optional: true}
      on_mismatch: approve | deny    # default approve
      max_calls_per_day: <n>         # allowed calls per UTC day; past it, on_mismatch applies
      allow_grants: true|false       # operators may /grant a bounded run of calls (never high risk)
```
