# WhatsApp intake

Draftcat can accept WhatsApp messages as governed pipeline input. The WhatsApp
session itself should live in a small receiver process built on whatsmeow; that
receiver posts normalized JSON into Draftcat's authenticated webhook. Draftcat
then owns the approval boundary, LLM budget, schema validation, state, and audit
trail.

The handoff JSON is deliberately small:

```json
{
  "id": "wamid.example",
  "from": "491701234567@s.whatsapp.net",
  "push_name": "DACH Service GmbH",
  "text": "Bitte schicken Sie uns ein Angebot.",
  "timestamp": "2026-07-04T08:00:00Z"
}
```

Pipeline shape:

```yaml
webhook:
  enabled: true
  addr: 0.0.0.0:8088
  secret_env: DRAFTCAT_WEBHOOK_SECRET

pipelines:
  - name: whatsapp-lead-intake
    schedule: webhook
    steps:
      - {name: normalize, type: deterministic, action: whatsapp_intake}
      - {name: triage, type: ai, skill: triage-lead}
      - {name: review, type: approval, mode: hitl, channel: telegram}
```

`whatsapp_intake` reads `webhook_body` or `input`, validates required fields, and
sets:

- `input` — prompt-ready message body
- `whatsapp_message` — structured normalized message
- `whatsapp_from` — sender JID/number
- `whatsapp_text` — original message text

The receiver can be replaced without changing the governed pipeline. A whatsmeow
sidecar only needs to keep the WhatsApp session connected and POST the normalized
payload with `Authorization: Bearer $DRAFTCAT_WEBHOOK_SECRET`.
