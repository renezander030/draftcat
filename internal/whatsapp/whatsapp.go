package whatsapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Message is the normalized handoff from a whatsmeow receiver into draftcat.
// Keep the shape small: the receiver owns WhatsApp session state; draftcat owns
// governed processing and approval.
type Message struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	PushName  string `json:"push_name"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

func ParseWebhook(raw string) (Message, error) {
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return msg, fmt.Errorf("parse whatsapp payload: %w", err)
	}
	msg.ID = strings.TrimSpace(msg.ID)
	msg.From = strings.TrimSpace(msg.From)
	msg.PushName = strings.TrimSpace(msg.PushName)
	msg.Text = strings.TrimSpace(msg.Text)
	msg.Timestamp = strings.TrimSpace(msg.Timestamp)
	if msg.From == "" {
		return msg, fmt.Errorf("whatsapp payload requires from")
	}
	if msg.Text == "" {
		return msg, fmt.Errorf("whatsapp payload requires text")
	}
	if msg.Timestamp == "" {
		msg.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	return msg, nil
}

func FormatForPrompt(msg Message) string {
	name := msg.PushName
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("WhatsApp inbound\nFrom: %s\nName: %s\nAt: %s\n\n%s", msg.From, name, msg.Timestamp, msg.Text)
}
