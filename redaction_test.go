package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/redact"
)

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestTelegramTransportErrorDoesNotLeakToken(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	const token = "123456789:AAHsecretTelegramToken"
	redact.Register("DRAFTCAT_TG_TOKEN", token)

	orig := tgClient
	tgClient = &http.Client{Transport: failingTransport{}}
	defer func() { tgClient = orig }()

	var logs bytes.Buffer
	log.SetOutput(redact.Writer(&logs))
	defer log.SetOutput(os.Stderr)

	bot := &TGBot{token: token, chatID: 1}
	err := bot.Send("hello")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("token leaked: err=%q log=%q", err, logs.String())
	}
	if !strings.Contains(err.Error(), "[REDACTED:DRAFTCAT_TG_TOKEN]") {
		t.Fatalf("error not labelled: %q", err)
	}
	if _, err := bot.getUpdates(); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("getUpdates error leaked or missing: %v", err)
	}
}

func TestOperatorNoticeIsRedacted(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	redact.Register("OPENROUTER_API_KEY", "sk-or-v1-abcdef0123456789")
	var sent string
	bot := &TGBot{chatID: 1, api: func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		sent, _ = payload["text"].(string)
		return json.RawMessage(`{}`), nil
	}}
	_ = bot.Send("[draftcat] ERROR in p: 401 invalid key sk-or-v1-abcdef0123456789")
	if strings.Contains(sent, "sk-or-v1-abcdef0123456789") || !strings.Contains(sent, "[REDACTED:OPENROUTER_API_KEY]") {
		t.Fatalf("notice = %q", sent)
	}
}

func TestRedactErrKeepsChain(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	redact.Register("K", "supersecretvalue")
	base := errors.New("bad supersecretvalue")
	err := redactErr(base)
	if strings.Contains(err.Error(), "supersecretvalue") || !errors.Is(err, base) {
		t.Fatalf("err=%q is=%v", err, errors.Is(err, base))
	}
	if redactErr(nil) != nil {
		t.Fatal("nil error wrapped")
	}
}

func TestRegisterSecretsCoversConfiguredVariables(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	t.Setenv("MY_PROVIDER_KEY", "provider-key-0001")
	t.Setenv("MY_HOOK_SECRET", "hook-secret-0002")
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", "approval-secret-0003")
	t.Setenv("MY_OTLP_HEADERS", "Authorization=Bearer otlp-token-0004")
	cfg := &config.Config{}
	cfg.Provider.APIKeyEnv = "MY_PROVIDER_KEY"
	cfg.Webhook.SecretEnv = "MY_HOOK_SECRET"
	cfg.Observ.OTLP.HeaderEnv = "MY_OTLP_HEADERS"
	cfg.Telegram.SetToken("resolved-token-0005")
	registerSecrets(cfg)
	in := "provider-key-0001 hook-secret-0002 approval-secret-0003 otlp-token-0004 resolved-token-0005"
	out := redact.String(in)
	for _, v := range strings.Fields(in) {
		if strings.Contains(out, v) {
			t.Errorf("%s not redacted: %s", v, out)
		}
	}
}
