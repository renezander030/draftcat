# Slack relay for draftcat

A conformant `hitl/v0` relay that puts draftcat approvals into a Slack channel
as Block Kit messages with **Approve**, **Skip** and **Adjust** buttons.

draftcat keeps the gate: policy, quorum, expiry, the payload hash and the audit
trail. This relay only presents the draft and reports who clicked what. It is a
single Go binary with no dependencies outside the standard library.

```
draftcat ──signed dispatch──▶ slack-relay ──chat.postMessage──▶ Slack channel
   ▲                              │  ▲                                │
   └────signed decision───────────┘  └──── signed button click ───────┘
```

## What it does

- **`POST /dispatch`** takes draftcat's dispatch envelope. It verifies the
  `X-Draftcat-Signature` HMAC and its ±5 minute window, the protocol version,
  that `payload_hash` is the SHA-256 of the draft it is about to show, that the
  approval has a callback URL and nonce, and that it has not expired. Then it
  posts the message to Slack and acknowledges. A dispatch that fails any check,
  or that Slack refuses, gets a non-2xx answer, so draftcat's dispatch fails at
  once instead of waiting for a timeout. A repeated dispatch for the same
  approval is acknowledged without posting twice.
- The message shows the pipeline, step, risk, expiry (in each reader's own time
  zone), the approvers in scope as Slack mentions, the day's spend when draftcat
  sends a budget, the draft verbatim as plain text (a draft containing
  `<!channel>` cannot ping anyone), and the payload hash. A draft longer than
  one Slack block is split into parts; one longer than a message can carry
  (36,000 characters) is cut, and the message says so and how much it shows.
  The payload hash line is always there. Buttons appear only for the decisions
  the dispatch offers.
- **`POST /slack/interactivity`** is the app's interactivity request URL. It
  verifies Slack's `X-Slack-Signature` (v0, HMAC-SHA256 over
  `v0:<timestamp>:<body>` with the signing secret, constant-time compare) and
  refuses a timestamp more than five minutes from now. A click from a Slack user
  who is not in `SLACK_APPROVERS`, or whose identity is outside the step's
  approver scope, gets an ephemeral "not allowed" note and nothing is sent.
  Otherwise the relay signs a decision envelope bound to the dispatch's
  approval id, nonce and payload hash, posts it to draftcat, and updates the
  message with the outcome, removing the buttons.
- A second click after a decision changes nothing: the clicker gets an
  ephemeral note and no second decision is sent. Two clicks at the same moment
  send one decision. With a quorum, the message stays open and shows who has
  approved so far until draftcat reports the gate resolved; one person clicking
  Approve twice is counted once.
- **Adjust** sends the text typed into the message's Adjust box as
  `adjust_text`. draftcat rewrites the draft with that note and asks again.
- An approval nobody decided is closed in Slack when it expires.
- **`GET /healthz`** answers `ok`.

Pending approvals live in memory. draftcat owns durability: if the relay
restarts, a click on an old message is answered "no longer open", and the gate
on draftcat's side times out, which does not fire the action.

## Slack app setup

1. Create an app at <https://api.slack.com/apps> (from scratch) in your
   workspace.
2. **OAuth & Permissions** → Bot Token Scopes: add `chat:write`. Install the
   app to the workspace and copy the **Bot User OAuth Token** (`xoxb-…`).
3. **Basic Information** → App Credentials: copy the **Signing Secret**.
4. **Interactivity & Shortcuts** → turn Interactivity on and set the Request URL
   to `https://<relay-host>/slack/interactivity`. Slack must reach it over
   HTTPS, so put the relay behind your reverse proxy or tunnel.
5. Invite the app to the approval channel (`/invite @your-app`) and copy the
   channel ID (channel details → bottom of the About tab, `C…`).
6. Find each approver's Slack member ID (profile → ⋯ → Copy member ID, `U…`).

## Environment

| Variable | Required | Meaning |
|---|---|---|
| `DRAFTCAT_RELAY_SECRET` | yes | The shared hitl/v0 secret, the same value draftcat reads through `relay.secret_env`. Verifies dispatches and signs decisions. |
| `SLACK_BOT_TOKEN` | yes | Bot token (`xoxb-…`) with `chat:write`. |
| `SLACK_SIGNING_SECRET` | yes | The app's signing secret, used to verify every click. |
| `SLACK_CHANNEL_ID` | yes | Channel the approvals are posted to (`C…`). |
| `SLACK_APPROVERS` | yes | Slack member IDs allowed to decide, separated by commas or spaces. Map each to the identity draftcat knows that operator by with `=`: `U012ABCDEF=alice@example.com,U034GHIJKL=bob@example.com`. An ID without `=` is its own identity. |
| `RELAY_ADDR` | no | Listen address, default `127.0.0.1:8090`. |
| `SLACK_API_URL` | no | Web API base, default `https://slack.com/api`. Must be https (plain http only to a loopback address). |

Secrets are read from the environment only and never logged.

## Run it

```bash
cd contrib/slack-relay
go build -o slack-relay .

export DRAFTCAT_RELAY_SECRET="the same secret draftcat has"
export SLACK_BOT_TOKEN="xoxb-…"
export SLACK_SIGNING_SECRET="…"
export SLACK_CHANNEL_ID="C0123456789"
export SLACK_APPROVERS="U012ABCDEF=alice@example.com,U034GHIJKL=bob@example.com"
./slack-relay
```

## draftcat config

Approvals reach Slack through draftcat's `relay` channel. The identities in
`relay.operators` are the ones `SLACK_APPROVERS` maps to.

```yaml
relay:
  url: http://127.0.0.1:8090/dispatch
  secret_env: DRAFTCAT_RELAY_SECRET
  callback_addr: "127.0.0.1:8089"
  public_url: http://127.0.0.1:8089     # where the relay posts decisions
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
  - name: agent-1
    steps:
      - name: send_email
        type: approval
        channel: relay
        approvers: [111, 222]
```

`public_url` only has to be reachable from the relay. Run on the same host, a
loopback address is enough; elsewhere, use an address the relay can reach.

## Verify before you trust it

Run the conformance suite against the relay. It dispatches a probe approval and
waits for a decision: click **Approve** on the message it posts in Slack, as a
user whose `SLACK_APPROVERS` identity you pass with `--identity`.

```bash
export DRAFTCAT_RELAY_SECRET="…"
draftcat hitl verify http://127.0.0.1:8090/dispatch --identity alice@example.com
```

Exit code 0 means conformant. Approving the probe executes nothing.

`go test ./...` in this directory runs the same suite end to end without Slack:
it builds draftcat from this repository (or uses `DRAFTCAT_BIN`), runs
`draftcat hitl verify` against the relay with a local stand-in for the Slack
Web API, and clicks Approve with a signed Slack request. `go test -short` skips
that test.

## Security model

The relay is untrusted. draftcat verifies every decision it receives (HMAC over
the body, the clock window, the single-use nonce, the payload-hash echo, the
approver's membership and the expiry), so a broken or hostile relay can only
stall an approval into a timeout, never authorise one. The relay's own checks,
the Slack signature and the approver list, keep anyone else in the workspace
from clicking on an operator's behalf.
