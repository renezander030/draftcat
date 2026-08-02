# Microsoft Teams relay for draftcat

A conformant `hitl/v0` relay that puts draftcat approvals into Teams as
Adaptive Cards, **without draftcat owning a bot**.

Two ways to run it. Pick the first unless you need in-place card updates.

| | Power Automate flow | Self-hosted relay |
|---|---|---|
| Azure app registration | none | none |
| Tenant admin consent | none | none |
| Where it runs | Microsoft's cloud | next to draftcat |
| Setup | ~10 min in a browser | `docker run` / one binary |
| Card updates in place | no (posts a result message) | no |

Both pass `draftcat hitl verify`.

## Why not a Teams bot

The obvious approaches do not work any more, which is what this directory
exists to route around:

- **Incoming webhooks / O365 connectors** — retired; connector webhooks were
  fully disabled in May 2026. They also never could receive a card submit.
- **Graph `chatMessage`** — can post an Adaptive Card but card actions other
  than `OpenUrl` require a bot. Send-only.
- **Graph Approvals API** (`/solutions/approval`) — beta, documented as not
  supported in production.
- **Bot Framework SDK (Python)** — archived; support ended 2025-12-31.
- **A registered Teams bot** — works, but costs an Azure app registration, a
  messaging endpoint, a Teams app manifest and tenant admin consent, per
  deployment.

The relay keeps all of that outside draftcat. See
[`docs/hitl-protocol.md`](../../docs/hitl-protocol.md) for the protocol.

---

## Option A — Power Automate flow (recommended)

`flow-definition.json` in this directory is an importable flow. If you would
rather build it by hand, it is three actions:

1. **Trigger: When an HTTP request is received.**
   Paste `schema/dispatch.schema.json` as the request body schema. Save, then
   copy the generated URL — that is `relay.url` in your draftcat config.

2. **Action: Post adaptive card and wait for a response** (Teams connector).
   - *Post as*: Flow bot · *Post in*: the approver's chat or channel
   - *Message*: paste `card.json`
   - This action suspends the flow until a human taps a button. Microsoft holds
     the wait, which is why draftcat's dispatch can return immediately.

3. **Action: HTTP** — POST the decision back.
   - *URI*: `@{triggerBody()?['callback']?['url']}`
   - *Headers*:
     - `Content-Type: application/json`
     - `X-Draftcat-Signature: @{outputs('Sign')}` (see signing below)
   - *Body*:

```json
{
  "protocol": "hitl/v0",
  "approval_id": "@{triggerBody()?['approval_id']}",
  "nonce": "@{triggerBody()?['callback']?['nonce']}",
  "decision": "@{body('Post_adaptive_card_and_wait_for_a_response')?['data']?['decision']}",
  "payload_hash": "@{triggerBody()?['payload_hash']}",
  "approver": {
    "id": "@{body('Post_adaptive_card_and_wait_for_a_response')?['responder']?['email']}",
    "display": "@{body('Post_adaptive_card_and_wait_for_a_response')?['responder']?['displayName']}",
    "channel": "teams"
  },
  "adjust_text": "@{body('Post_adaptive_card_and_wait_for_a_response')?['data']?['adjust_text']}",
  "decided_at": "@{utcNow()}"
}
```

### Signing in Power Automate

draftcat requires `X-Draftcat-Signature: t=<unix>,v1=<hex hmac-sha256(t + "." + body)>`.
Power Automate has no HMAC expression, so use one **Inline JavaScript** action
(`Sign`) before the HTTP step, or run Option B, which signs for you.

If your tenant blocks inline code actions, use Option B. Do **not** work around
this by disabling signature verification: the signature is what stops a captured
request from being replayed, and the payload hash it covers is what proves the
human approved *that* draft.

---

## Option B — self-hosted relay

A ~200-line Go binary that speaks `hitl/v0` on one side and the Teams **Graph
API** on the other, using an existing user or app token you already have. It
signs correctly, verifies draftcat's dispatch signature, and needs no inline
code action.

```bash
cd contrib/teams-relay
go build -o teams-relay .

export DRAFTCAT_RELAY_SECRET="the same secret draftcat has"
export TEAMS_WEBHOOK_URL="https://…"     # any endpoint that renders the card
export RELAY_ADDR="0.0.0.0:8090"
./teams-relay
```

Then point draftcat at it:

```yaml
relay:
  url: http://127.0.0.1:8090/dispatch
  secret_env: DRAFTCAT_RELAY_SECRET
  callback_addr: "127.0.0.1:8089"
  public_url: https://gate.example.com
```

---

## Verify before you trust it

```bash
export DRAFTCAT_RELAY_SECRET="…"
draftcat hitl verify https://prod-42.westeurope.logic.azure.com/workflows/…
```

The suite dispatches a real approval, waits for your relay to answer, and checks
that the decision is signed, in-window, echoes the payload hash, and that a
replay, a mutated hash and an out-of-scope approver are all refused. Approving
the probe executes nothing.

Exit code 0 means conformant. Anything else prints which check failed.
