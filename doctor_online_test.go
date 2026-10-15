package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/redact"
)

// fakeRemote answers like the Telegram Bot API and an OpenRouter-compatible
// provider: goodToken and goodKey are accepted, anything else is refused.
func fakeRemote(t *testing.T, goodToken, goodKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bot"+goodToken+"/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"draftcat_test_bot"}}`))
		case r.URL.Path == "/bot"+goodToken+"/getChat":
			if r.URL.Query().Get("chat_id") == "42" {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"type":"private"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
		case strings.HasPrefix(r.URL.Path, "/bot"):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
		case r.URL.Path == "/api/v1/key":
			if r.Header.Get("Authorization") != "Bearer "+goodKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prev := telegramAPIBase
	telegramAPIBase = srv.URL
	t.Cleanup(func() { telegramAPIBase = prev })
	t.Cleanup(redact.Reset)
	return srv
}

func onlineConfig(t *testing.T, srvURL string) string {
	t.Helper()
	cfg := strings.Replace(doctorConfig, "provider:\n  type: openrouter\n", "provider:\n  type: openrouter\n  base_url: "+srvURL+"/api/v1\n", 1)
	p := writeDoctorConfig(t, "unused.db")
	if err := os.WriteFile(p, []byte(strings.Replace(cfg, "STATE", "unused.db", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctorOnlineAcceptsWorkingCredentials(t *testing.T) {
	srv := fakeRemote(t, "123:good-token-value", "sk-good-provider-key")
	t.Setenv("DOC_TG_TOKEN", "123:good-token-value")
	t.Setenv("DOC_PROVIDER_KEY", "sk-good-provider-key")
	rep := doctor(onlineConfig(t, srv.URL), t.TempDir(), time.Now(), true)
	for _, name := range []string{"telegram api", "telegram chat", "provider api"} {
		if c := checkByName(rep, name); c.Status != "ok" {
			t.Fatalf("%s: %+v", name, c)
		}
	}
	if c := checkByName(rep, "telegram api"); !strings.Contains(c.Detail, "@draftcat_test_bot") {
		t.Fatalf("getMe detail: %+v", c)
	}
}

func TestDoctorOnlineNamesRejectedCredentialsWithoutPrintingThem(t *testing.T) {
	srv := fakeRemote(t, "123:good-token-value", "sk-good-provider-key")
	t.Setenv("DOC_TG_TOKEN", "999:revoked-token-value")
	t.Setenv("DOC_PROVIDER_KEY", "sk-wrong-provider-key")
	t.Setenv("DRAFTCAT_STATE_PATH", "")
	cfgPath := onlineConfig(t, srv.URL)
	var out bytes.Buffer
	code := runDoctor([]string{"--config", cfgPath, "--skills", t.TempDir(), "--online", "--json"}, &out)
	if code != 1 {
		t.Fatalf("rejected credentials must fail doctor, exit %d", code)
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if c := checkByName(rep, "telegram api"); c.Status != "fail" || !strings.Contains(c.Detail, "Unauthorized") {
		t.Fatalf("telegram: %+v", c)
	}
	if c := checkByName(rep, "provider api"); c.Status != "fail" || !strings.Contains(c.Detail, "HTTP 401") {
		t.Fatalf("provider: %+v", c)
	}
	for _, secret := range []string{"999:revoked-token-value", "sk-wrong-provider-key"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("doctor printed a credential: %s", out.String())
		}
	}
}

func TestDoctorOnlineRedactsTransportErrors(t *testing.T) {
	telegramPrev := telegramAPIBase
	telegramAPIBase = "http://127.0.0.1:1" // nothing listens: the error quotes the URL
	t.Cleanup(func() { telegramAPIBase = telegramPrev; redact.Reset() })
	t.Setenv("DOC_TG_TOKEN", "123:secret-token-in-url")
	t.Setenv("DOC_PROVIDER_KEY", "sk-unused-key")
	rep := doctor(onlineConfig(t, "http://127.0.0.1:1"), t.TempDir(), time.Now(), true)
	c := checkByName(rep, "telegram api")
	if c.Status != "fail" || strings.Contains(c.Detail, "secret-token-in-url") || !strings.Contains(c.Detail, "[REDACTED:DOC_TG_TOKEN]") {
		t.Fatalf("transport error must be redacted: %+v", c)
	}
}

func TestDoctorOfflineMakesNoCalls(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	prev := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = prev }()
	t.Setenv("DOC_TG_TOKEN", "123:abc")
	t.Setenv("DOC_PROVIDER_KEY", "sk-test")
	var out bytes.Buffer
	_ = runDoctor([]string{"--config", onlineConfig(t, srv.URL), "--skills", t.TempDir()}, &out)
	if hits != 0 {
		t.Fatalf("doctor without --online contacted a remote %d time(s)", hits)
	}
}
