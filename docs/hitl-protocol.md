# HITL Approval Protocol (`hitl/v0`)

A vendor-neutral wire protocol for putting a human between an AI agent and a
real-world action. draftcat implements the **gate** side. Anything that can
receive an HTTP POST and send one back can implement the **relay** side.

Status: draft. Transport-stable, field-stable within `v0`.

---

## 1. Why a protocol instead of a Teams adapter

Issue #5 asked for a Microsoft Teams channel. The obvious implementation is not
available, and the reasons generalise to every vendor surface:

| Transport | Verdict (researched 2026-07) |
|---|---|
| Teams incoming webhooks / O365 connectors | **Retired** — connector webhooks fully disabled May 2026 |
| Graph `chatMessage` | Send-only. Card actions other than `OpenUrl` require a bot; no submit path back |
| Graph Approvals API (`/solutions/approval`) | Beta, documented as not supported in production |
| Bot Framework SDK (Python) | Archived; support ended 2025-12-31 |
| Registered Teams bot (Action.Execute) | Works — but costs an Azure app registration, a messaging endpoint, a Teams app manifest, and **tenant admin consent** |

So "add `teams.go`" is not a small file. It is draftcat taking ownership of a
bot lifecycle inside somebody else's tenant, and repeating that work per vendor.

`internal/channels` exists precisely to stop a channel from being *configurable*
without being *implemented*: an approval gate that routes somewhere the operator
is not watching is worse than no gate, because it still looks like it held.
Adding `teams` as a half-channel would reintroduce that trap.

The protocol resolves both problems at once. draftcat ships **one** new fully
round-trip-capable channel — `relay` — and the vendor-specific work moves
outside the binary, into a component the operator's own tenant already trusts.

**Separation of concerns**

- **Gate** (draftcat) owns what must not be delegated: policy, quorum, expiry,
  the payload hash, the audit trail, signed receipts.
- **Relay** (a Power Automate flow, a bot, n8n, a shell script) owns only
  presentation: render the request to a human, return exactly one decision.

The relay is **untrusted**. It cannot manufacture an approval — see §5.

---

## 2. Roles and flow

```
  pipeline step                                    human
       |                                             ^
       v                                             |
  +----------+   1. dispatch (signed)   +---------+  |  2. render
  |   GATE   | -----------------------> |  RELAY  | -+
  | draftcat |                          | (vendor)|
  |          | <----------------------- +---------+
  +----------+   3. decision (signed)
       |
       v
  audit record + receipt
```

1. **Dispatch** — gate POSTs an approval request to the relay's URL. The relay
   MUST acknowledge with `2xx` promptly and MUST NOT hold the connection open
   for the human. Approvals take hours; connections do not.
2. **Render** — the relay shows the draft to a human on whatever surface it owns.
3. **Callback** — the relay POSTs one decision back to the gate.

The gate remains authoritative throughout. A dispatch that is never answered
expires on the gate's own clock and resolves as `timeout` — identical to today's
behavior. Restarting draftcat does not lose a pending gate; it is already
durable in `pending_approvals`.

---

## 3. Dispatch envelope (gate → relay)

`POST <relay_url>`
`Content-Type: application/json`

```json
{
  "protocol": "hitl/v0",
  "approval_id": "01J8Z7...",
  "run_id": "01J8Z6...",
  "pipeline": "invoice-due-diligence",
  "step": "send-followup",
  "issued_at": "2026-08-02T09:14:22Z",
  "expires_at": "2026-08-02T13:14:22Z",
  "risk": "normal",
  "quorum": { "required": 2, "collected": 0 },
  "approvers": ["alice@example.com", "bob@example.com"],
  "payload_hash": "sha256:9f2b...",
  "draft": {
    "content_type": "text/plain",
    "body": "Hi Anna, following up on invoice 2026-114..."
  },
  "budget": {
    "unit": "EUR",
    "spent_today": 4.12,
    "cap_today": 20.00,
    "spent_pipeline": 0.38,
    "cap_pipeline": 2.00
  },
  "actions": [
    { "verb": "approve" },
    { "verb": "skip" },
    { "verb": "adjust", "accepts": "text" }
  ],
  "callback": {
    "url": "https://gate.example.com/hitl/v0/decisions",
    "nonce": "b7c1e4..."
  }
}
```

Field notes:

- `run_id` correlates the approval to the pipeline run in the audit trail.
- `payload_hash` is the SHA-256 of the exact bytes in `draft.body`. It is the
  anchor for the whole security model.
- `budget` is advisory context so the human decides with the day's remaining
  spend in front of them. Omitted when no cap is configured.
- `risk` is the gate's own classification of the step. A relay MAY use it to
  style the prompt; it MUST NOT use it to skip asking.
- `approvers` is the scoped subset permitted to decide this step. The relay
  SHOULD only present to these people; the gate enforces it regardless.
- `actions` is a closed set in `v0`: `approve`, `skip`, `adjust`.

### Headers

```
X-Hitl-Protocol:       hitl/v0
X-Draftcat-Signature:  t=<unix>,v1=<hex hmac-sha256(t + "." + body)>
```

The signature is HMAC-SHA256 over `<timestamp> + "." + <raw body>`, keyed with
the shared relay secret, and the timestamp travels inside the same header.

This is deliberately the identical header and construction draftcat already
uses for inbound webhook triggers, rather than a second scheme invented for this
protocol. One signing scheme in the binary means one thing to get right, one
thing to test, and one thing for a relay author to implement. It is body-bound,
so a captured header cannot be replayed against a different body, and the
timestamp bounds how long a captured request stays useful at all.

Reference implementation of both sides: `internal/relay/protocol.go`
(`Sign` / `VerifySignature`).

---

## 4. Decision envelope (relay → gate)

`POST /hitl/v0/decisions`

```json
{
  "protocol": "hitl/v0",
  "approval_id": "01J8Z7...",
  "nonce": "b7c1e4...",
  "decision": "approve",
  "payload_hash": "sha256:9f2b...",
  "approver": {
    "id": "alice@example.com",
    "display": "Alice Reuter",
    "channel": "teams"
  },
  "adjust_text": null,
  "decided_at": "2026-08-02T09:31:05Z"
}
```

Same headers, same signature scheme, same secret.

`decision` is one of `approve`, `skip`, `adjust`. A relay never sends `timeout`
— expiry is the gate's call, on the gate's clock. A relay that could declare a
timeout could also starve a gate into one.

For `quorum.required > 1` the relay posts **one decision per approver**. The
gate counts distinct approver identities and resolves when the count is met.

### Gate response

- `200` `{"status":"recorded","resolved":true|false}` — accepted. `resolved`
  reports whether this decision closed the gate or quorum still needs more.
- `409` `{"status":"already_resolved"}` — the gate is closed. Idempotent: the
  relay may safely retry and will keep getting `409`.
- `410` `{"status":"expired"}` — the gate timed out before the human answered.
- `401` / `403` — signature, nonce, approver, or payload-hash check failed.

---

## 5. Security model — why an untrusted relay is safe

The relay sees the draft and reports who decided. It is not trusted with
anything else. Five independent checks stand between it and a forged approval:

1. **HMAC over the body.** A relay cannot post a decision without the shared
   secret, and cannot alter a decision after signing it.
2. **Timestamp window.** Decisions outside a ±5 minute skew are refused, so a
   captured request has a bounded replay life.
3. **Single-use nonce.** Issued at dispatch, bound to that one `approval_id`,
   burned on first use. Replay inside the window still fails.
4. **Payload-hash echo.** The relay must echo the hash it was given. A relay
   that showed the human a different draft than the gate staged cannot produce
   a matching hash, and the decision is refused. This is what makes "the human
   approved *this* text" verifiable rather than asserted.
5. **Approver membership.** The gate checks `approver.id` against its own
   `allowed_users`, narrowed by the step's `approvers` scope. A relay cannot
   invent an approver, and cannot widen who may decide.

A compromised relay can therefore **deny** (drop dispatches, never answer — the
gate times out and the action does not fire, which is the safe direction) but
cannot **authorise**.

Every accepted decision is written to `action_approvals` with the approver
identity, the payload hash, the nonce, and the existing HMAC receipt, so
`draftcat audit-verify` covers relay decisions with no change in kind.

---

## 6. Reaching Microsoft Teams with no bot

One Power Automate flow, owned by the customer's own tenant:

1. **Trigger:** *When an HTTP request is received* — this URL is `relay_url`.
2. **Action:** *Post adaptive card and wait for a response* (Teams connector) —
   post the card into the approver's chat or channel. The flow suspends until a
   human submits. Microsoft holds the wait, not draftcat.
3. **Action:** *HTTP* — POST the decision envelope to
   `https://<gate-host>/hitl/v0/decisions` with the three signature headers.

What this buys:

- **No Azure app registration, no bot, no tenant admin consent for draftcat.**
  The Teams connector authenticates as the flow owner, who is already an
  employee with a Teams licence.
- The card is a normal Adaptive Card with Approve / Skip / Adjust. `Adjust`
  posts the edited text back in `adjust_text`; the gate re-hashes and treats it
  as an operator edit, exactly as the Telegram channel does today.
- No dependency on retired connector webhooks or the archived Bot Framework SDK.

A registered Teams bot using `Action.Execute` remains a valid relay for anyone
who wants in-place card updates — but it becomes *their* deployment choice, not
draftcat's dependency. Slack, Discord, email, PagerDuty and a plain CLI script
are the same three steps against a different presenter.

---

## 7. Conformance

A protocol nobody can test is a document. `draftcat hitl verify <relay_url>`
dispatches a synthetic approval against a live relay and asserts:

- the relay acks `2xx` without holding the connection,
- a decision comes back signed, within the window, with a burnable nonce,
- the `payload_hash` echo matches,
- a replayed decision is refused,
- a decision from an out-of-scope approver is refused,
- a decision carrying a mutated payload hash is refused.

A relay that passes is conformant. That is the whole bar — implementable in a
Power Automate flow, in forty lines of any language, or in an existing bot.

---

## 8. Configuration

```yaml
relay:
  url: https://prod-42.westeurope.logic.azure.com/workflows/...
  secret_env: DRAFTCAT_RELAY_SECRET
  callback_addr: "127.0.0.1:8089"
  public_url: https://gate.example.com
  operators:
    - id: 111
      identity: alice@example.com
    - id: 222
      identity: bob@example.com
  security:
    allowed_users: [111, 222]
    max_input_length: 500
    rate_limit: 10

pipelines:
  - name: invoice-due-diligence
    steps:
      - name: send-followup
        type: approval
        channel: relay
        quorum: 2
        approvers: [111, 222]
```

Two representations of an operator exist on purpose. `id` is the numeric
operator identity quorum counting, approver scoping and the audit trail were all
built on; `identity` is what travels on the wire and what the relay reports
back. The gate maps one to the other, so an unknown identity is never admitted
and the audit trail keeps recording the same operator IDs it always has.

`relay` is a first-class implemented channel in `internal/channels`: it ships a
complete `OperatorChannel` round trip including the callback server, so naming
it in a config is never a lie about what the engine will do. draftcat runs one
operator channel per process — a configured relay takes the gate, and a relay
that fails to start is fatal rather than a silent fallback to Telegram.

Conformance: `draftcat hitl verify <relay-url>` (see §7).

---

## 9. Compatibility

`v0` is additive. Unknown fields MUST be ignored by both sides, so the gate can
add context (a new `budget` key, a new `risk` level) without breaking deployed
relays. A breaking change bumps the `protocol` string and the path segment
together: `hitl/v1`, `/hitl/v1/decisions`.
