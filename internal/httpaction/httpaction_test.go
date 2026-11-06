package httpaction

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, vars map[string]string) Spec {
	t.Helper()
	s, errs := Parse(vars)
	if len(errs) > 0 {
		t.Fatalf("parse %v: %v", vars, errs)
	}
	return s
}

func TestParseDefaults(t *testing.T) {
	s := mustParse(t, map[string]string{"url": "https://example.com/hook"})
	if s.Method != http.MethodPost || s.Timeout != DefaultTimeout || s.MaxResponseBytes != DefaultMaxResponseBytes || s.BodyKey != DefaultBodyKey {
		t.Fatalf("defaults: %+v", s)
	}
	if !s.Expected(200) || !s.Expected(299) || s.Expected(300) || s.Expected(404) {
		t.Fatal("default expect_status must be any 2xx")
	}
	if g := mustParse(t, map[string]string{"url": "https://example.com", "method": "get"}); g.Method != http.MethodGet || g.BodyKey != "" {
		t.Fatalf("GET sends no body by default: %+v", g)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"url":                {},
		"url scheme":         {"url": "ftp://example.com/x"},
		"url host":           {"url": "https:///path"},
		"url userinfo":       {"url": "https://user:pw@example.com/"},
		"method":             {"url": "https://example.com", "method": "TRACE"},
		"body and body_key":  {"url": "https://example.com", "body": "{}", "body_key": "x"},
		"get with body":      {"url": "https://example.com", "method": "GET", "body": "{}"},
		"auth header name":   {"url": "https://example.com", "auth_env": "TOKEN", "auth_header": "Bad Header"},
		"auth scheme":        {"url": "https://example.com", "auth_env": "TOKEN", "auth_scheme": "Two Words"},
		"auth env name":      {"url": "https://example.com", "auth_env": "not-a-var"},
		"static credential":  {"url": "https://example.com", "headers": "Authorization: Bearer abc"},
		"static custom auth": {"url": "https://example.com", "auth_env": "TOKEN", "auth_header": "X-Api-Key", "headers": "X-Api-Key: abc"},
		"idempotency header": {"url": "https://example.com", "headers": "Idempotency-Key: fixed"},
		"malformed header":   {"url": "https://example.com", "headers": "no colon here"},
		"timeout":            {"url": "https://example.com", "timeout": "soon"},
		"zero timeout":       {"url": "https://example.com", "timeout": "0s"},
		"expect_status":      {"url": "https://example.com", "expect_status": "200,abc"},
		"max_response_bytes": {"url": "https://example.com", "max_response_bytes": "-1"},
	}
	for name, vars := range cases {
		if _, errs := Parse(vars); len(errs) == 0 {
			t.Errorf("%s: expected an error for %v", name, vars)
		}
	}
}

func TestPlainRemote(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://example.com/x":    true,
		"http://10.0.0.5/x":       true,
		"http://localhost:8080/x": false,
		"http://127.0.0.1/x":      false,
		"http://[::1]:9000/x":     false,
		"https://example.com/x":   false,
	} {
		if got := mustParse(t, map[string]string{"url": raw}).PlainRemote(); got != want {
			t.Errorf("%s: PlainRemote=%v, want %v", raw, got, want)
		}
	}
}

func TestIdempotencyKeyStable(t *testing.T) {
	a, b := IdempotencyKey("run-1", "send"), IdempotencyKey("run-1", "send")
	if a != b || a == "" {
		t.Fatalf("same run and step must give the same key: %q %q", a, b)
	}
	if a == IdempotencyKey("run-2", "send") || a == IdempotencyKey("run-1", "other") {
		t.Fatal("another run or step must give another key")
	}
}

func TestBodyRendering(t *testing.T) {
	data := map[string]interface{}{
		"ai_output": map[string]interface{}{"to": "agent-1", "note": "a < b"},
		"ai_raw":    `say "hi"`,
		"count":     3,
	}
	s := mustParse(t, map[string]string{"url": "https://example.com"})
	b, err := Body(s, data)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if json.Unmarshal(b, &got) != nil || got["to"] != "agent-1" || !strings.Contains(string(b), "a < b") {
		t.Fatalf("default body = %s", b)
	}

	tpl := mustParse(t, map[string]string{"url": "https://example.com", "body": `{"text": {{ai_raw}}, "n": {{ count }}}`})
	b, err = Body(tpl, data)
	if err != nil || string(b) != `{"text": "say \"hi\"", "n": 3}` {
		t.Fatalf("template body = %s, %v", b, err)
	}

	if _, err := Body(tpl, map[string]interface{}{"count": 1}); err == nil || !strings.Contains(err.Error(), "ai_raw") {
		t.Fatalf("missing data must refuse: %v", err)
	}
	bad := mustParse(t, map[string]string{"url": "https://example.com", "body": `{"text": {{count}}`})
	if _, err := Body(bad, data); err == nil {
		t.Fatal("invalid JSON body must refuse")
	}
	text := mustParse(t, map[string]string{"url": "https://example.com", "headers": "Content-Type: text/plain", "body": "note: {{ai_raw}}"})
	if b, err := Body(text, data); err != nil || string(b) != `note: say "hi"` {
		t.Fatalf("text body = %s, %v", b, err)
	}
	raw := mustParse(t, map[string]string{"url": "https://example.com", "body_key": "ai_raw"})
	if b, err := Body(raw, map[string]interface{}{"ai_raw": ` {"a":1}`}); err != nil || string(b) != ` {"a":1}` {
		t.Fatalf("a JSON document in a string is sent as that document: %s, %v", b, err)
	}
	if b, err := Body(raw, map[string]interface{}{"ai_raw": "plain"}); err != nil || string(b) != `"plain"` {
		t.Fatalf("plain text in a JSON body is a JSON string: %s, %v", b, err)
	}
	if _, err := Body(s, map[string]interface{}{}); err == nil {
		t.Fatal("no ai_output must refuse rather than send an empty body")
	}
}

func TestDescribeNamesCredentialWithoutValue(t *testing.T) {
	s := mustParse(t, map[string]string{"url": "https://example.com/x", "auth_env": "ORDERS_TOKEN", "headers": "X-Source: agent-1"})
	d := Describe(s, []byte(`{"a":1}`), "k1")
	for _, want := range []string{"POST https://example.com/x", "Authorization: Bearer <credential from ORDERS_TOKEN>", "X-Source: agent-1", "Idempotency-Key: k1", `{"a":1}`} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	if Digest(s, []byte(`{"a":1}`), "k1") == Digest(s, []byte(`{"a":2}`), "k1") {
		t.Fatal("digest must change with the body")
	}
}

func TestDoSuccessJSON(t *testing.T) {
	const token = "tok-0123456789abcdef"
	var gotAuth, gotKey, gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey, gotType = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/orders/9007199254740993")
		w.Header().Set("Set-Cookie", "s=1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 9007199254740993, "ok": true}`))
	}))
	defer srv.Close()
	s := mustParse(t, map[string]string{"url": srv.URL + "/orders", "auth_env": "ORDERS_TOKEN"})
	resp, err := Do(context.Background(), s, []byte(`{"a":1}`), "key-1", token)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token || gotKey != "key-1" || gotBody != `{"a":1}` || gotType != "application/json" {
		t.Fatalf("request: auth=%q key=%q type=%q body=%q", gotAuth, gotKey, gotType, gotBody)
	}
	obj, ok := resp.JSON.(map[string]interface{})
	if resp.Status != 201 || !ok || obj["id"].(json.Number).String() != "9007199254740993" {
		t.Fatalf("response: %+v", resp)
	}
	if resp.Headers["Location"] == "" || resp.Headers["Set-Cookie"] != "" {
		t.Fatalf("headers kept: %v", resp.Headers)
	}
	d := resp.Data("key-1")
	if d["status"] != 201 || d["idempotency_key"] != "key-1" || d["truncated"] != false {
		t.Fatalf("data: %v", d)
	}
}

func TestDoCustomAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Authorization")
	}))
	defer srv.Close()
	s := mustParse(t, map[string]string{"url": srv.URL, "auth_env": "K", "auth_header": "X-Api-Key"})
	if _, err := Do(context.Background(), s, nil, "k", "secret-value-123"); err != nil {
		t.Fatal(err)
	}
	if got != "secret-value-123|" {
		t.Fatalf("custom header carries the raw credential and no Authorization: %q", got)
	}
}

func TestDoNon2xxFailsWithRedactedExcerpt(t *testing.T) {
	const token = "tok-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token " + token + " " + strings.Repeat("x", 2000)))
	}))
	defer srv.Close()
	s := mustParse(t, map[string]string{"url": srv.URL, "auth_env": "ORDERS_TOKEN"})
	_, err := Do(context.Background(), s, []byte(`{}`), "k", token)
	if err == nil {
		t.Fatal("401 must fail the step")
	}
	msg := err.Error()
	if strings.Contains(msg, token) || !strings.Contains(msg, "[REDACTED:ORDERS_TOKEN]") || !strings.Contains(msg, "HTTP 401") {
		t.Fatalf("error = %q", msg)
	}
	if len(msg) > excerptBytes+200 {
		t.Fatalf("excerpt not bounded: %d bytes", len(msg))
	}
}

func TestDoExpectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if _, err := Do(context.Background(), mustParse(t, map[string]string{"url": srv.URL, "expect_status": "200,201"}), nil, "k", ""); err == nil {
		t.Fatal("202 is not in 200,201")
	}
	if _, err := Do(context.Background(), mustParse(t, map[string]string{"url": srv.URL, "expect_status": "202"}), nil, "k", ""); err != nil {
		t.Fatal(err)
	}
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s := mustParse(t, map[string]string{"url": srv.URL, "auth_env": "K", "auth_header": "X-Api-Key"})
	if _, err := Do(context.Background(), s, []byte(`{}`), "k", "secret-value-123"); err == nil || hit {
		t.Fatalf("a redirect must fail the step and never reach the other host: err=%v hit=%v", err, hit)
	}
}

func TestDoTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	s := mustParse(t, map[string]string{"url": srv.URL, "timeout": "50ms"})
	start := time.Now()
	_, err := Do(context.Background(), s, []byte(`{}`), "k", "")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied")
	}
}

func TestDoResponseBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("a", 4096) + `"}`))
	}))
	defer srv.Close()
	s := mustParse(t, map[string]string{"url": srv.URL, "max_response_bytes": "100"})
	resp, err := Do(context.Background(), s, []byte(`{}`), "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.Text) != 100 || resp.JSON != nil {
		t.Fatalf("bounded response: truncated=%v len=%d json=%v", resp.Truncated, len(resp.Text), resp.JSON)
	}
}

func TestUnknownVars(t *testing.T) {
	got := UnknownVars(map[string]string{"url": "x", "auth_evn": "T", "extra": "1"})
	if len(got) != 2 || got[0] != "auth_evn" || got[1] != "extra" {
		t.Fatalf("unknown vars = %v", got)
	}
}
