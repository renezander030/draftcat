package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestVerifySlackSignature(t *testing.T) {
	secret := []byte(testSigningSecret)
	body := []byte("payload=%7B%22type%22%3A%22block_actions%22%7D")
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	good := slackSign(secret, ts, body)

	if err := verifySlackSignature(secret, ts, good, body, now); err != nil {
		t.Fatalf("good signature refused: %v", err)
	}
	if err := verifySlackSignature(secret, ts, good, body, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("a 4 minute old request is inside the window: %v", err)
	}
	refused := map[string]error{
		"wrong secret":    verifySlackSignature([]byte("other"), ts, good, body, now),
		"tampered body":   verifySlackSignature(secret, ts, good, append([]byte("x"), body...), now),
		"bad signature":   verifySlackSignature(secret, ts, "v0=00", body, now),
		"stale":           verifySlackSignature(secret, ts, good, body, now.Add(6*time.Minute)),
		"from the future": verifySlackSignature(secret, ts, good, body, now.Add(-6*time.Minute)),
		"missing sig":     verifySlackSignature(secret, ts, "", body, now),
		"missing ts":      verifySlackSignature(secret, "", good, body, now),
		"garbage ts":      verifySlackSignature(secret, "soon", good, body, now),
		"other version":   verifySlackSignature(secret, ts, strings.Replace(good, "v0=", "v1=", 1), body, now),
	}
	for name, err := range refused {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDraftcatSignatureRoundTrip(t *testing.T) {
	secret := []byte(testRelaySecret)
	body := []byte(`{"protocol":"hitl/v0"}`)
	now := time.Now()
	h := sign(secret, body, now)
	if err := verifySignature(secret, h, body, now); err != nil {
		t.Fatal(err)
	}
	if verifySignature(secret, h, []byte(`{"protocol":"hitl/v1"}`), now) == nil {
		t.Fatal("signature must be bound to the body")
	}
	if verifySignature(secret, h, body, now.Add(6*time.Minute)) == nil {
		t.Fatal("signature must expire")
	}
}

func TestDraftSplitsAtSlackLimit(t *testing.T) {
	body := strings.Repeat("All good here. ", 600) // 9000 characters
	blocks := toMaps(draftBlocks(body))
	var joined strings.Builder
	for _, b := range blocks {
		if b["type"] != "section" {
			continue
		}
		text, _ := b["text"].(map[string]any)
		if text["type"] != "plain_text" {
			t.Fatalf("draft sections must be plain_text, got %v", text["type"])
		}
		s, _ := text["text"].(string)
		if utf16Len(s) > maxSectionText {
			t.Fatalf("section of %d units exceeds Slack's %d", utf16Len(s), maxSectionText)
		}
		joined.WriteString(s)
	}
	if joined.String() != body {
		t.Fatal("splitting must not lose or reorder text")
	}
	if !strings.Contains(allText(blocks), "shown in 3 parts") {
		t.Error("a split draft says so")
	}
}

func TestChunkingCountsUTF16(t *testing.T) {
	body := strings.Repeat("😀", 2000) // 4000 UTF-16 units
	chunks := chunkText(body, maxSectionText)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) || utf16Len(c) > maxSectionText {
			t.Fatalf("chunk invalid or too long: %d units", utf16Len(c))
		}
	}
	if strings.Join(chunks, "") != body {
		t.Fatal("chunks must reassemble to the input")
	}
}

func TestLongDraftIsTruncatedWithMarkerAndKeepsHash(t *testing.T) {
	var d dispatch
	d.Protocol, d.ApprovalID, d.Pipeline, d.Step = protocolVer, "approval-1", "agent-1", "send_email"
	d.ExpiresAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	d.Draft.Body = strings.Repeat("x", 100_000)
	d.PayloadHash = hashPayload(d.Draft.Body)
	d.Actions = []action{{Verb: verbApprove}, {Verb: verbSkip}, {Verb: verbAdjust}}
	d.Callback.Nonce, d.Callback.URL = "n", "http://127.0.0.1:1/hitl/v0/decisions"

	blocks := toMaps(renderBlocks(&d, view{buttons: true}, func(s string) string { return s }))
	if len(blocks) > 50 {
		t.Fatalf("%d blocks exceeds Slack's 50", len(blocks))
	}
	text := allText(blocks)
	if !strings.Contains(text, "Draft truncated") || !strings.Contains(text, "36000 of 100000") {
		t.Error("truncation must be marked with what was shown")
	}
	if !strings.Contains(text, d.PayloadHash) {
		t.Fatal("the payload hash line must survive truncation")
	}
	shown := 0
	for _, b := range blocksOfType(blocks, "section") {
		if tx, ok := b["text"].(map[string]any); ok && tx["type"] == "plain_text" {
			shown += utf16Len(tx["text"].(string))
		}
	}
	if shown != maxDraftShown {
		t.Fatalf("shown %d characters, want %d", shown, maxDraftShown)
	}
	raw, _ := json.Marshal(blocks)
	if len(raw) > 45_000 {
		t.Fatalf("message payload %d bytes is past what Slack accepts", len(raw))
	}
}

func TestDraftAndNamesCannotPing(t *testing.T) {
	var d dispatch
	d.Protocol, d.ApprovalID = protocolVer, "approval-1"
	d.Pipeline, d.Step = "<!channel>", "<https://example.com|click>"
	d.Draft.Body = "<!everyone> please read"
	d.PayloadHash = hashPayload(d.Draft.Body)
	d.Callback.Nonce = "n"
	d.Actions = []action{{Verb: verbApprove}}

	blocks := toMaps(renderBlocks(&d, view{buttons: true}, func(s string) string { return s }))
	text := allText(blocks)
	if strings.Contains(text, "<!channel>") || strings.Contains(text, "<https://") {
		t.Error("mrkdwn fields must escape control syntax")
	}
	found := false
	for _, b := range blocksOfType(blocks, "section") {
		if tx, ok := b["text"].(map[string]any); ok && tx["type"] == "plain_text" && tx["text"] == d.Draft.Body {
			found = true
		}
	}
	if !found {
		t.Error("the draft is shown verbatim as plain_text")
	}
}

func TestSlackClientErrorsNeverCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
	}))
	defer srv.Close()
	c := &slackClient{base: srv.URL, token: testBotToken, http: srv.Client()}

	_, err := c.postMessage(context.Background(), "C1", "t", nil)
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("want the Slack error code, got %v", err)
	}
	err2 := c.update(context.Background(), "C1", "1.2", "t", nil)
	if err2 == nil || !strings.Contains(err2.Error(), "HTTP 500") {
		t.Fatalf("want HTTP 500, got %v", err2)
	}
	for _, e := range []error{err, err2} {
		if strings.Contains(e.Error(), testBotToken) {
			t.Fatal("an error leaked the bot token")
		}
	}
}

func TestParseApprovers(t *testing.T) {
	got, err := parseApprovers("U012ABC=alice@example.com, W034DEF\nU056GHI=bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"U012ABC": "alice@example.com", "W034DEF": "W034DEF", "U056GHI": "bob@example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
	for name, raw := range map[string]string{
		"not a user id":      "alice@example.com",
		"lower case id":      "u012abc",
		"empty identity":     "U012ABC=",
		"same user twice":    "U012ABC=a@example.com,U012ABC=b@example.com",
		"shared identity":    "U012ABC=a@example.com,U034DEF=a@example.com",
		"nothing configured": " , ",
	} {
		if _, err := parseApprovers(raw); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"DRAFTCAT_RELAY_SECRET": "s", "SLACK_BOT_TOKEN": "xoxb-1", "SLACK_SIGNING_SECRET": "ss",
		"SLACK_CHANNEL_ID": "C1", "SLACK_APPROVERS": "U012ABC",
	}
	get := func(k string) string { return env[k] }
	cfg, err := loadConfig(get)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8090" || cfg.APIBase != defaultAPIBase {
		t.Errorf("defaults: %+v", cfg)
	}

	_, err = loadConfig(func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "SLACK_SIGNING_SECRET") || !strings.Contains(err.Error(), "DRAFTCAT_RELAY_SECRET") {
		t.Fatalf("missing env should be named: %v", err)
	}

	env["SLACK_API_URL"] = "http://slack.example.com/api"
	if _, err := loadConfig(get); err == nil {
		t.Error("the bot token must not travel over plain http to a remote host")
	}
	env["SLACK_API_URL"] = "http://127.0.0.1:9999/api"
	if _, err := loadConfig(get); err != nil {
		t.Errorf("loopback http is a local test double: %v", err)
	}
}

func toMaps(blocks []any) []map[string]any {
	raw, _ := json.Marshal(blocks)
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
