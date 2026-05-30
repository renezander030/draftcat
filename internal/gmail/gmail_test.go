package gmail

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// --- ExtractEmailAddress ---

func TestExtractEmailAddressWithBrackets(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"John Doe <john@example.com>", "john@example.com"},
		{"<alice@test.org>", "alice@test.org"},
		{`"Bob Smith" <bob@corp.io>`, "bob@corp.io"},
	}
	for _, tc := range cases {
		got := ExtractEmailAddress(tc.input)
		if got != tc.expected {
			t.Errorf("ExtractEmailAddress(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestExtractEmailAddressPlain(t *testing.T) {
	// No angle brackets — return as-is
	got := ExtractEmailAddress("plain@email.com")
	if got != "plain@email.com" {
		t.Errorf("expected 'plain@email.com', got %q", got)
	}
}

func TestExtractEmailAddressEmpty(t *testing.T) {
	got := ExtractEmailAddress("")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// --- timeAgo ---

func TestTimeAgoJustNow(t *testing.T) {
	result := timeAgo(time.Now())
	if result != "just now" {
		t.Errorf("expected 'just now', got %q", result)
	}
}

func TestTimeAgoMinutes(t *testing.T) {
	result := timeAgo(time.Now().Add(-5 * time.Minute))
	if !strings.HasSuffix(result, "m ago") {
		t.Errorf("expected '<N>m ago', got %q", result)
	}
}

func TestTimeAgoHours(t *testing.T) {
	result := timeAgo(time.Now().Add(-3 * time.Hour))
	if !strings.HasSuffix(result, "h ago") {
		t.Errorf("expected '<N>h ago', got %q", result)
	}
}

func TestTimeAgoDays(t *testing.T) {
	result := timeAgo(time.Now().Add(-2 * 24 * time.Hour))
	if !strings.HasSuffix(result, "d ago") {
		t.Errorf("expected '<N>d ago', got %q", result)
	}
}

func TestTimeAgoOlderThanWeek(t *testing.T) {
	result := timeAgo(time.Now().Add(-14 * 24 * time.Hour))
	// Should return date format like "Mar 6"
	if strings.HasSuffix(result, "ago") {
		t.Errorf("expected date format for >7 days, got %q", result)
	}
}

// --- FormatEmailsForPrompt ---

func TestFormatEmailsForPromptEmpty(t *testing.T) {
	result := FormatEmailsForPrompt(nil)
	if result != "No emails found." {
		t.Errorf("expected 'No emails found.', got %q", result)
	}
}

func TestFormatEmailsForPromptSingle(t *testing.T) {
	emails := []Email{
		{
			From:    "John Doe <john@example.com>",
			To:      "Me <me@test.com>",
			Subject: "Test Subject",
			Date:    "2h ago",
			Snippet: "This is a snippet",
		},
	}
	result := FormatEmailsForPrompt(emails)
	if !strings.Contains(result, "John Doe") {
		t.Error("expected 'John Doe' in output")
	}
	if !strings.Contains(result, "Test Subject") {
		t.Error("expected 'Test Subject' in output")
	}
	if !strings.Contains(result, "2h ago") {
		t.Error("expected '2h ago' in output")
	}
	if !strings.Contains(result, "This is a snippet") {
		t.Error("expected snippet in output")
	}
	// Should start with "1."
	if !strings.HasPrefix(result, "1.") {
		t.Errorf("expected output to start with '1.', got %q", result[:10])
	}
}

func TestFormatEmailsForPromptMultiple(t *testing.T) {
	emails := []Email{
		{From: "A <a@test.com>", To: "B <b@test.com>", Subject: "First", Date: "1h ago", Snippet: "snip1"},
		{From: "C <c@test.com>", To: "D <d@test.com>", Subject: "Second", Date: "2h ago", Snippet: "snip2"},
	}
	result := FormatEmailsForPrompt(emails)
	if !strings.Contains(result, "1.") {
		t.Error("expected '1.' numbering")
	}
	if !strings.Contains(result, "2.") {
		t.Error("expected '2.' numbering")
	}
}

func TestFormatEmailsForPromptLongSubjectTruncated(t *testing.T) {
	longSubject := strings.Repeat("x", 100)
	emails := []Email{
		{From: "A <a@t.com>", To: "B <b@t.com>", Subject: longSubject, Date: "1h ago", Snippet: "s"},
	}
	result := FormatEmailsForPrompt(emails)
	if strings.Contains(result, longSubject) {
		t.Error("expected subject to be truncated at 80 chars")
	}
	if !strings.Contains(result, "...") {
		t.Error("expected '...' for truncated subject")
	}
}

func TestFormatEmailsForPromptCleansFromField(t *testing.T) {
	emails := []Email{
		{From: `"Quoted Name" <q@test.com>`, To: `"Rec" <r@test.com>`, Subject: "S", Date: "1h ago", Snippet: "s"},
	}
	result := FormatEmailsForPrompt(emails)
	// Should strip quotes from name
	if strings.Contains(result, `"Quoted Name"`) {
		t.Error("expected quotes to be stripped from From name")
	}
	if !strings.Contains(result, "Quoted Name") {
		t.Error("expected cleaned name 'Quoted Name'")
	}
}

// --- FormatThreadForPrompt ---

func TestFormatThreadForPromptDirectionLabels(t *testing.T) {
	emails := []Email{
		{From: "Me <me@test.com>", To: "Other <other@test.com>", Date: "2024-01-01 10:00", Body: "Hi there"},
		{From: "Other <other@test.com>", To: "Me <me@test.com>", Date: "2024-01-01 10:30", Body: "Hello back"},
	}
	result := FormatThreadForPrompt(emails, "me@test.com")
	if !strings.Contains(result, "[SENT]") {
		t.Error("expected [SENT] label for own email")
	}
	if !strings.Contains(result, "[RECEIVED]") {
		t.Error("expected [RECEIVED] label for other's email")
	}
}

func TestFormatThreadForPromptCleansName(t *testing.T) {
	emails := []Email{
		{From: `"John" <john@test.com>`, To: "me@test.com", Date: "2024-01-01", Body: "Hi"},
	}
	result := FormatThreadForPrompt(emails, "me@test.com")
	if strings.Contains(result, `"John"`) {
		t.Error("expected quotes to be stripped from name in thread")
	}
	if !strings.Contains(result, "John") {
		t.Error("expected name 'John' in output")
	}
}

func TestFormatThreadForPromptEmpty(t *testing.T) {
	result := FormatThreadForPrompt(nil, "me@test.com")
	if result != "" {
		t.Errorf("expected empty string for nil emails, got %q", result)
	}
}

func TestFormatThreadForPromptCaseInsensitive(t *testing.T) {
	emails := []Email{
		{From: "Me <ME@TEST.COM>", To: "other@test.com", Date: "2024-01-01", Body: "test"},
	}
	result := FormatThreadForPrompt(emails, "me@test.com")
	if !strings.Contains(result, "[SENT]") {
		t.Error("expected case-insensitive address matching for SENT label")
	}
}

// --- BuildReplyRaw ---

func TestBuildReplyRawAddsRePrefix(t *testing.T) {
	raw := BuildReplyRaw("to@test.com", "Original Subject", "<msg-id>", "<refs>", "Reply body")
	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}
	msg := string(decoded)
	if !strings.Contains(msg, "Subject: Re: Original Subject") {
		t.Error("expected 'Re:' prefix added to subject")
	}
}

func TestBuildReplyRawNoDoubleRe(t *testing.T) {
	raw := BuildReplyRaw("to@test.com", "Re: Already replied", "<msg-id>", "<refs>", "Reply body")
	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)
	if strings.Contains(msg, "Re: Re:") {
		t.Error("expected no double 'Re:' prefix")
	}
	if !strings.Contains(msg, "Subject: Re: Already replied") {
		t.Errorf("expected subject preserved, got: %s", msg)
	}
}

func TestBuildReplyRawHeaders(t *testing.T) {
	raw := BuildReplyRaw("recipient@test.com", "Test", "<msg-123>", "<ref-456>", "Hello")
	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)
	if !strings.Contains(msg, "To: recipient@test.com") {
		t.Error("expected To header")
	}
	if !strings.Contains(msg, "In-Reply-To: <msg-123>") {
		t.Error("expected In-Reply-To header")
	}
	if !strings.Contains(msg, "References: <ref-456>") {
		t.Error("expected References header")
	}
	if !strings.Contains(msg, "Content-Type: text/plain; charset=UTF-8") {
		t.Error("expected Content-Type header")
	}
}

func TestBuildReplyRawBody(t *testing.T) {
	raw := BuildReplyRaw("to@test.com", "Sub", "<id>", "<ref>", "This is the reply body")
	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)
	if !strings.Contains(msg, "This is the reply body") {
		t.Error("expected body content in message")
	}
}

func TestBuildReplyRawIsValidBase64(t *testing.T) {
	raw := BuildReplyRaw("to@test.com", "Test", "<id>", "<ref>", "body")
	_, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Errorf("expected valid URL-safe base64, got error: %v", err)
	}
}

func TestBuildReplyRawCaseInsensitiveRe(t *testing.T) {
	// "re:" lowercase should also not get double prefix
	raw := BuildReplyRaw("to@test.com", "re: lowercase", "<id>", "<ref>", "body")
	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)
	if strings.Contains(msg, "Re: re:") {
		t.Error("expected case-insensitive Re: check")
	}
}
