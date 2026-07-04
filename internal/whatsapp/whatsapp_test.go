package whatsapp

import (
	"strings"
	"testing"
)

func TestParseWebhookNormalizesWhatsAppMessage(t *testing.T) {
	msg, err := ParseWebhook(`{
		"id": " wamid.1 ",
		"from": " 491701234567@s.whatsapp.net ",
		"push_name": " Dach Service ",
		"text": " Bitte Angebot schicken. ",
		"timestamp": "2026-07-04T08:00:00Z"
	}`)
	if err != nil {
		t.Fatalf("ParseWebhook failed: %v", err)
	}
	if msg.ID != "wamid.1" || msg.From != "491701234567@s.whatsapp.net" || msg.PushName != "Dach Service" {
		t.Fatalf("message not normalized: %#v", msg)
	}
	if msg.Text != "Bitte Angebot schicken." {
		t.Fatalf("text not trimmed: %q", msg.Text)
	}
	body := FormatForPrompt(msg)
	for _, want := range []string{"WhatsApp inbound", "491701234567@s.whatsapp.net", "Dach Service", "Bitte Angebot schicken."} {
		if !strings.Contains(body, want) {
			t.Fatalf("formatted prompt missing %q in %q", want, body)
		}
	}
}

func TestParseWebhookRejectsMissingRequiredFields(t *testing.T) {
	if _, err := ParseWebhook(`{"text":"hi"}`); err == nil || !strings.Contains(err.Error(), "from") {
		t.Fatalf("expected missing from error, got %v", err)
	}
	if _, err := ParseWebhook(`{"from":"491701234567@s.whatsapp.net"}`); err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("expected missing text error, got %v", err)
	}
}
