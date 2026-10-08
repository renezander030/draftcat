package redact

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestStringReplacesRegisteredValues(t *testing.T) {
	Reset()
	defer Reset()
	Register("DRAFTCAT_TG_TOKEN", "123456:ABCDEF-secret-token")
	Register("OPENROUTER_API_KEY", "sk-or-v1-0123456789")
	in := `Post "https://api.telegram.org/bot123456:ABCDEF-secret-token/sendMessage": dial tcp; key sk-or-v1-0123456789`
	got := String(in)
	if strings.Contains(got, "ABCDEF-secret-token") || strings.Contains(got, "sk-or-v1-0123456789") {
		t.Fatalf("secret survived redaction: %s", got)
	}
	if !strings.Contains(got, "bot[REDACTED:DRAFTCAT_TG_TOKEN]/sendMessage") || !strings.Contains(got, "[REDACTED:OPENROUTER_API_KEY]") {
		t.Fatalf("unexpected redaction: %s", got)
	}
}

func TestShortAndDuplicateValuesIgnored(t *testing.T) {
	Reset()
	defer Reset()
	Register("A", "short")
	Register("B", "  ")
	Register("C", "long-enough-value")
	Register("D", "long-enough-value")
	if Count() != 1 {
		t.Fatalf("Count = %d, want 1", Count())
	}
	if got := String("a short note"); got != "a short note" {
		t.Fatalf("short value redacted: %q", got)
	}
}

func TestLongestValueWins(t *testing.T) {
	Reset()
	defer Reset()
	Register("INNER", "abcdefgh")
	Register("OUTER", "abcdefgh-ijklmnop")
	if got := String("x abcdefgh-ijklmnop y"); got != "x [REDACTED:OUTER] y" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterRedactsLogLines(t *testing.T) {
	Reset()
	defer Reset()
	Register("DRAFTCAT_WEBHOOK_SECRET", "webhook-secret-value")
	var buf bytes.Buffer
	l := log.New(Writer(&buf), "", 0)
	l.Printf("auth failed for bearer %s", "webhook-secret-value")
	if strings.Contains(buf.String(), "webhook-secret-value") {
		t.Fatalf("log line leaked secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED:DRAFTCAT_WEBHOOK_SECRET]") {
		t.Fatalf("log line not redacted: %s", buf.String())
	}
}

func TestWriterReportsCallerLength(t *testing.T) {
	Reset()
	defer Reset()
	Register("K", "0123456789")
	var buf bytes.Buffer
	n, err := Writer(&buf).Write([]byte("k=0123456789\n"))
	if err != nil || n != len("k=0123456789\n") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
