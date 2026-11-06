package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRelaySecret   = "relay-secret-for-tests"
	testSigningSecret = "signing-secret-for-tests"
	testBotToken      = "test-bot-token"
	testChannel       = "C0TEST"
	testTS            = "1700000000.000100"
)

// --- fake Slack Web API ---

type slackCall struct {
	method string
	auth   string
	ctype  string
	body   map[string]any
}

type fakeSlack struct {
	mu     sync.Mutex
	calls  []slackCall
	fail   map[string]string // method -> Slack error code
	posted chan struct{}
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/")
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, slackCall{method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
	code := f.fail[method]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if code != "" {
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + code + `"}`))
		return
	}
	if method == "chat.postMessage" {
		_, _ = w.Write([]byte(`{"ok":true,"channel":"` + testChannel + `","ts":"` + testTS + `"}`))
		select {
		case f.posted <- struct{}{}:
		default:
		}
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (f *fakeSlack) byMethod(m string) []slackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []slackCall
	for _, c := range f.calls {
		if c.method == m {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- fake draftcat callback ---

type gatePost struct {
	body  []byte
	sig   string
	proto string
}

type fakeGate struct {
	mu      sync.Mutex
	posts   []gatePost
	respond func(n int) (int, string) // n = 1-based post count
	arrived chan struct{}
	release chan struct{} // when set, each post waits for it
}

func (g *fakeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.posts = append(g.posts, gatePost{raw, r.Header.Get(sigHeader), r.Header.Get(protoHeader)})
	n := len(g.posts)
	respond, release := g.respond, g.release
	g.mu.Unlock()
	select {
	case g.arrived <- struct{}{}:
	default:
	}
	if release != nil {
		<-release
	}
	code, body := http.StatusOK, `{"status":"recorded","resolved":true}`
	if respond != nil {
		code, body = respond(n)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func (g *fakeGate) all() []gatePost {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gatePost(nil), g.posts...)
}

// --- harness ---

type harness struct {
	t     *testing.T
	rs    *relayServer
	relay *httptest.Server
	slack *fakeSlack
	sapi  *httptest.Server
	gate  *fakeGate
	gsrv  *httptest.Server
}

var testApprovers = map[string]string{
	"U0ALICE": "alice@example.com",
	"U0BOB":   "bob@example.com",
	"U0CAROL": "carol@example.com",
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, testApprovers)
}

func newHarnessWith(t *testing.T, approvers map[string]string) *harness {
	t.Helper()
	h := &harness{t: t}
	h.slack = &fakeSlack{fail: map[string]string{}, posted: make(chan struct{}, 8)}
	h.sapi = httptest.NewServer(h.slack)
	h.gate = &fakeGate{arrived: make(chan struct{}, 8)}
	h.gsrv = httptest.NewServer(h.gate)
	h.rs = newRelay(config{
		RelaySecret:   []byte(testRelaySecret),
		BotToken:      testBotToken,
		SigningSecret: []byte(testSigningSecret),
		Channel:       testChannel,
		Approvers:     approvers,
		APIBase:       h.sapi.URL,
	})
	h.relay = httptest.NewServer(h.rs.routes())
	t.Cleanup(func() {
		h.rs.wg.Wait()
		h.relay.Close()
		h.gsrv.Close()
		h.sapi.Close()
	})
	return h
}

func (h *harness) dispatch() dispatch {
	var d dispatch
	d.Protocol = protocolVer
	d.ApprovalID = "approval-1"
	d.RunID = "run-1"
	d.Pipeline = "agent-1"
	d.Step = "send_email"
	d.IssuedAt = time.Now().UTC().Format(time.RFC3339)
	d.ExpiresAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	d.Quorum = quorum{Required: 1}
	d.Approvers = []string{"alice@example.com", "bob@example.com"}
	d.Draft.ContentType = "text/plain"
	d.Draft.Body = "Hello, the report for example.com is attached."
	d.PayloadHash = hashPayload(d.Draft.Body)
	d.Actions = []action{{Verb: verbApprove}, {Verb: verbSkip}, {Verb: verbAdjust, Accepts: "text"}}
	d.Callback.URL = h.gsrv.URL + "/hitl/v0/decisions"
	d.Callback.Nonce = "nonce-1"
	return d
}

// send POSTs a dispatch signed with secret at time at.
func (h *harness) send(d dispatch, secret string, at time.Time) int {
	h.t.Helper()
	body, err := json.Marshal(d)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.sendRaw(body, sign([]byte(secret), body, at))
}

func (h *harness) sendRaw(body []byte, sig string) int {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.relay.URL+"/dispatch", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set(sigHeader, sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) post(d dispatch) {
	h.t.Helper()
	if code := h.send(d, testRelaySecret, time.Now()); code != http.StatusOK {
		h.t.Fatalf("dispatch: HTTP %d", code)
	}
}

// clickBody is a block_actions payload the way Slack sends it.
func clickBody(user, verb, approvalID, adjustText, ts string) []byte {
	p := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": user, "username": strings.ToLower(user), "name": strings.ToLower(user)},
		"channel":   map[string]any{"id": testChannel},
		"container": map[string]any{"type": "message", "channel_id": testChannel, "message_ts": ts},
		"actions":   []any{map[string]any{"action_id": actionPrefix + verb, "block_id": blockDecide, "value": approvalID, "type": "button"}},
		"state": map[string]any{"values": map[string]any{
			blockAdjust: map[string]any{actionAdjustT: map[string]any{"type": "plain_text_input", "value": adjustText}},
		}},
	}
	raw, _ := json.Marshal(p)
	return []byte(url.Values{"payload": {string(raw)}}.Encode())
}

// clickAsync sends a signed click and returns once Slack has been answered;
// the relay may still be forwarding it.
func (h *harness) clickAsync(user, verb, approvalID, adjustText string) int {
	h.t.Helper()
	body := clickBody(user, verb, approvalID, adjustText, testTS)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return h.sendInteraction(body, ts, slackSign([]byte(testSigningSecret), ts, body))
}

// click sends a signed click and waits until the relay has finished with it.
func (h *harness) click(user, verb, approvalID, adjustText string) {
	h.t.Helper()
	if code := h.clickAsync(user, verb, approvalID, adjustText); code != http.StatusOK {
		h.t.Fatalf("interaction: HTTP %d", code)
	}
	h.rs.wg.Wait()
}

func (h *harness) sendInteraction(body []byte, ts, sig string) int {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.relay.URL+"/slack/interactivity", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ts != "" {
		req.Header.Set(slackTSHeader, ts)
	}
	if sig != "" {
		req.Header.Set(slackSigHeader, sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// --- block helpers ---

func blocksOf(c slackCall) []map[string]any {
	raw, _ := c.body["blocks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if m, ok := b.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func blocksOfType(blocks []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, b := range blocks {
		if b["type"] == typ {
			out = append(out, b)
		}
	}
	return out
}

// allText flattens every text in the blocks, for "does the message say X".
func allText(blocks []map[string]any) string {
	raw, _ := json.Marshal(blocks)
	var sb strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, ok := x["text"].(string); ok {
				sb.WriteString(t)
				sb.WriteString("\n")
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	walk(v)
	return sb.String()
}

func buttonValues(blocks []map[string]any) map[string]string {
	out := map[string]string{}
	for _, b := range blocksOfType(blocks, "actions") {
		els, _ := b["elements"].([]any)
		for _, e := range els {
			m, _ := e.(map[string]any)
			id, _ := m["action_id"].(string)
			v, _ := m["value"].(string)
			out[id] = v
		}
	}
	return out
}
