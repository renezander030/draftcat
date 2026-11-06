package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- dispatch verification ---

func TestDispatchPostsBlockKitMessage(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)

	posts := h.slack.byMethod("chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("want one chat.postMessage, got %d", len(posts))
	}
	c := posts[0]
	if c.auth != "Bearer "+testBotToken {
		t.Errorf("Authorization = %q", c.auth)
	}
	if !strings.HasPrefix(c.ctype, "application/json") {
		t.Errorf("Content-Type = %q", c.ctype)
	}
	if c.body["channel"] != testChannel || c.body["unfurl_links"] != false {
		t.Errorf("channel/unfurl wrong: %v", c.body)
	}
	blocks := blocksOf(c)
	got := buttonValues(blocks)
	for _, verb := range []string{"approve", "skip", "adjust"} {
		if got[actionPrefix+verb] != d.ApprovalID {
			t.Errorf("button %s carries %q, want the approval id", verb, got[actionPrefix+verb])
		}
	}
	if len(blocksOfType(blocks, "input")) != 1 {
		t.Error("adjust is offered, so the message needs its text box")
	}
	text := allText(blocks)
	if !strings.Contains(text, d.PayloadHash) {
		t.Error("the payload hash line is missing")
	}
	if !strings.Contains(text, d.Draft.Body) {
		t.Error("the draft is missing")
	}
	if !strings.Contains(text, "<@U0ALICE>") || !strings.Contains(text, "<@U0BOB>") {
		t.Error("approvers in scope should be mentioned")
	}
}

func TestDispatchOffersOnlyTheVerbsItCarries(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	d.Actions = []action{{Verb: verbApprove}, {Verb: verbSkip}, {Verb: "escalate"}}
	h.post(d)

	blocks := blocksOf(h.slack.byMethod("chat.postMessage")[0])
	got := buttonValues(blocks)
	if len(got) != 2 || got[actionPrefix+verbAdjust] != "" {
		t.Fatalf("want approve and skip only, got %v", got)
	}
	if len(blocksOfType(blocks, "input")) != 0 {
		t.Error("no adjust offered, so no text box")
	}
}

func TestDispatchVerification(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h *harness, d *dispatch)
		secret string
		at     time.Duration
		want   int
	}{
		{name: "wrong secret", secret: "not-the-secret", want: http.StatusUnauthorized},
		{name: "stale signature", at: -10 * time.Minute, want: http.StatusUnauthorized},
		{name: "future signature", at: 10 * time.Minute, want: http.StatusUnauthorized},
		{name: "payload hash does not match draft", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.PayloadHash = hashPayload("a different draft") }},
		{name: "already expired", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) }},
		{name: "unparseable expiry", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.ExpiresAt = "tomorrow" }},
		{name: "other protocol", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.Protocol = "hitl/v9" }},
		{name: "no callback url", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.Callback.URL = "" }},
		{name: "callback is not http", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.Callback.URL = "file:///etc/passwd" }},
		{name: "no approval id", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.ApprovalID = "" }},
		{name: "no verb this relay knows", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.Actions = []action{{Verb: "escalate"}} }},
		{name: "actions without a nonce", want: http.StatusBadRequest,
			mutate: func(_ *harness, d *dispatch) { d.Callback.Nonce = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.dispatch()
			if tc.mutate != nil {
				tc.mutate(h, &d)
			}
			secret := tc.secret
			if secret == "" {
				secret = testRelaySecret
			}
			if code := h.send(d, secret, time.Now().Add(tc.at)); code != tc.want {
				t.Fatalf("HTTP %d, want %d", code, tc.want)
			}
			if n := h.slack.count(); n != 0 {
				t.Fatalf("a refused dispatch must not reach Slack; %d call(s)", n)
			}
		})
	}
}

func TestDispatchWithoutSignatureIsRefused(t *testing.T) {
	h := newHarness(t)
	body, _ := json.Marshal(h.dispatch())
	if code := h.sendRaw(body, ""); code != http.StatusUnauthorized {
		t.Fatalf("HTTP %d, want 401", code)
	}
	if code := h.sendRaw(body, "t=abc,v1=00"); code != http.StatusUnauthorized {
		t.Fatalf("malformed header: HTTP %d, want 401", code)
	}
}

func TestDispatchTwicePostsOnce(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	h.post(d) // a retry, or a replay inside the signature window
	if n := len(h.slack.byMethod("chat.postMessage")); n != 1 {
		t.Fatalf("want one Slack message, got %d", n)
	}
	d.Callback.Nonce = "nonce-2"
	if code := h.send(d, testRelaySecret, time.Now()); code != http.StatusConflict {
		t.Fatalf("same approval id, new nonce: HTTP %d, want 409", code)
	}
}

func TestDispatchFailsWhenSlackRefuses(t *testing.T) {
	h := newHarness(t)
	h.slack.fail["chat.postMessage"] = "channel_not_found"
	d := h.dispatch()
	if code := h.send(d, testRelaySecret, time.Now()); code != http.StatusBadGateway {
		t.Fatalf("HTTP %d, want 502 so draftcat's dispatch fails at once", code)
	}
	// Nothing was remembered: once Slack recovers, the same dispatch posts.
	delete(h.slack.fail, "chat.postMessage")
	h.post(d)
	if n := len(h.slack.byMethod("chat.postMessage")); n != 2 {
		t.Fatalf("want a second post attempt, got %d", n)
	}
}

func TestNotificationHasNoButtons(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	d.ApprovalID, d.ExpiresAt, d.Actions = "", "", nil
	d.Callback.URL, d.Callback.Nonce = "", ""
	h.post(d)
	blocks := blocksOf(h.slack.byMethod("chat.postMessage")[0])
	if len(blocksOfType(blocks, "actions")) != 0 || len(blocksOfType(blocks, "input")) != 0 {
		t.Fatal("a notification asks for no decision")
	}
	if !strings.Contains(allText(blocks), d.PayloadHash) {
		t.Fatal("the payload hash line is missing")
	}
}

// --- interactivity ---

func TestInteractionSignature(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	body := clickBody("U0ALICE", verbApprove, d.ApprovalID, "", testTS)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	old := strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10)

	cases := map[string]struct{ ts, sig string }{
		"bad signature":   {now, slackSign([]byte("wrong-secret"), now, body)},
		"stale timestamp": {old, slackSign([]byte(testSigningSecret), old, body)},
		"no signature":    {now, ""},
		"no timestamp":    {"", slackSign([]byte(testSigningSecret), now, body)},
	}
	for name, tc := range cases {
		if code := h.sendInteraction(body, tc.ts, tc.sig); code != http.StatusUnauthorized {
			t.Errorf("%s: HTTP %d, want 401", name, code)
		}
	}
	h.rs.wg.Wait()
	if n := len(h.gate.all()); n != 0 {
		t.Fatalf("an unverified click must not reach draftcat; %d decision(s) sent", n)
	}
}

func TestApproveSendsSignedDecision(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	h.click("U0ALICE", verbApprove, d.ApprovalID, "")

	posts := h.gate.all()
	if len(posts) != 1 {
		t.Fatalf("want one decision, got %d", len(posts))
	}
	p := posts[0]
	if err := verifySignature([]byte(testRelaySecret), p.sig, p.body, time.Now()); err != nil {
		t.Fatalf("decision signature: %v", err)
	}
	if p.proto != protocolVer {
		t.Errorf("%s = %q", protoHeader, p.proto)
	}
	var got map[string]any
	if err := json.Unmarshal(p.body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"protocol": "hitl/v0", "approval_id": d.ApprovalID, "nonce": d.Callback.Nonce,
		"decision": "approve", "payload_hash": d.PayloadHash,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	approver, _ := got["approver"].(map[string]any)
	if approver["id"] != "alice@example.com" || approver["channel"] != "slack" || approver["display"] != "u0alice" {
		t.Errorf("approver = %v", approver)
	}
	if _, has := got["adjust_text"]; has {
		t.Error("approve carries no adjust_text")
	}
	if ts, _ := got["decided_at"].(string); ts == "" {
		t.Error("decided_at missing")
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("decided_at %q: %v", ts, err)
	}

	updates := h.slack.byMethod("chat.update")
	if len(updates) != 1 {
		t.Fatalf("want one chat.update, got %d", len(updates))
	}
	u := updates[0]
	if u.body["channel"] != testChannel || u.body["ts"] != testTS {
		t.Errorf("update targets %v/%v", u.body["channel"], u.body["ts"])
	}
	blocks := blocksOf(u)
	if len(blocksOfType(blocks, "actions")) != 0 || len(blocksOfType(blocks, "input")) != 0 {
		t.Error("the decided message must lose its buttons")
	}
	if text := allText(blocks); !strings.Contains(text, "Approved") || !strings.Contains(text, "<@U0ALICE>") {
		t.Errorf("outcome missing from %q", text)
	}
}

func TestAdjustCarriesTheText(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)

	h.click("U0BOB", verbAdjust, d.ApprovalID, "   ")
	if n := len(h.gate.all()); n != 0 {
		t.Fatalf("adjust with an empty box must not be sent; got %d", n)
	}
	if e := h.slack.byMethod("chat.postEphemeral"); len(e) != 1 || e[0].body["user"] != "U0BOB" {
		t.Fatalf("the clicker should be told to describe the change: %+v", e)
	}

	h.click("U0BOB", verbAdjust, d.ApprovalID, "Shorter, and drop the second paragraph.")
	posts := h.gate.all()
	if len(posts) != 1 {
		t.Fatalf("want one decision, got %d", len(posts))
	}
	var got decision
	_ = json.Unmarshal(posts[0].body, &got)
	if got.Decision != verbAdjust || got.AdjustText != "Shorter, and drop the second paragraph." || got.Approver.ID != "bob@example.com" {
		t.Fatalf("decision = %+v", got)
	}
}

func TestNonApproverIsToldAndNothingIsSent(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	h.click("U0MALLORY", verbApprove, d.ApprovalID, "")

	if n := len(h.gate.all()); n != 0 {
		t.Fatalf("a non-approver's click reached draftcat (%d)", n)
	}
	e := h.slack.byMethod("chat.postEphemeral")
	if len(e) != 1 || e[0].body["user"] != "U0MALLORY" || !strings.Contains(e[0].body["text"].(string), "not an approver") {
		t.Fatalf("want one ephemeral 'not allowed' to the clicker, got %+v", e)
	}
	if n := len(h.slack.byMethod("chat.update")); n != 0 {
		t.Fatal("the message must not change")
	}
}

func TestApproverOutsideTheStepScope(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch() // scope: alice and bob
	h.post(d)
	h.click("U0CAROL", verbApprove, d.ApprovalID, "")
	if n := len(h.gate.all()); n != 0 {
		t.Fatalf("an approver outside the step's scope reached draftcat (%d)", n)
	}
	if e := h.slack.byMethod("chat.postEphemeral"); len(e) != 1 || !strings.Contains(e[0].body["text"].(string), "this step") {
		t.Fatalf("want an ephemeral about the step scope, got %+v", e)
	}
}

func TestSecondClickChangesNothing(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	h.click("U0ALICE", verbApprove, d.ApprovalID, "")
	h.click("U0ALICE", verbApprove, d.ApprovalID, "")
	h.click("U0BOB", verbSkip, d.ApprovalID, "")

	if n := len(h.gate.all()); n != 1 {
		t.Fatalf("want exactly one decision, got %d", n)
	}
	if n := len(h.slack.byMethod("chat.update")); n != 1 {
		t.Fatalf("want the message updated once, got %d", n)
	}
	if n := len(h.slack.byMethod("chat.postEphemeral")); n != 2 {
		t.Fatalf("each late click gets an ephemeral note; got %d", n)
	}
}

func TestConcurrentClicksSendOneDecision(t *testing.T) {
	h := newHarness(t)
	h.gate.release = make(chan struct{})
	d := h.dispatch()
	h.post(d)

	h.clickAsync("U0ALICE", verbApprove, d.ApprovalID, "")
	<-h.gate.arrived // the first decision is now in flight
	h.clickAsync("U0BOB", verbApprove, d.ApprovalID, "")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.slack.byMethod("chat.postEphemeral")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(h.gate.release)
	h.rs.wg.Wait()

	if n := len(h.gate.all()); n != 1 {
		t.Fatalf("two clicks at once must send one decision, got %d", n)
	}
	if e := h.slack.byMethod("chat.postEphemeral"); len(e) != 1 || e[0].body["user"] != "U0BOB" {
		t.Fatalf("the second clicker should be told a decision is in flight: %+v", e)
	}
}

func TestQuorumKeepsTheMessageOpen(t *testing.T) {
	h := newHarness(t)
	h.gate.respond = func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, `{"status":"recorded","resolved":false,"collected":1,"required":2}`
		}
		return http.StatusOK, `{"status":"recorded","resolved":true}`
	}
	d := h.dispatch()
	d.Quorum.Required = 2
	h.post(d)

	h.click("U0ALICE", verbApprove, d.ApprovalID, "")
	first := h.slack.byMethod("chat.update")
	if len(first) != 1 || len(buttonValues(blocksOf(first[0]))) != 3 {
		t.Fatal("after one of two approvals the buttons must stay")
	}
	if !strings.Contains(allText(blocksOf(first[0])), "1 of 2") {
		t.Error("progress should be shown")
	}

	h.click("U0ALICE", verbApprove, d.ApprovalID, "") // same person again
	if n := len(h.gate.all()); n != 1 {
		t.Fatalf("one person approving twice must not be sent twice; got %d", n)
	}

	h.click("U0BOB", verbApprove, d.ApprovalID, "")
	if n := len(h.gate.all()); n != 2 {
		t.Fatalf("want two decisions, got %d", n)
	}
	updates := h.slack.byMethod("chat.update")
	last := blocksOf(updates[len(updates)-1])
	if len(buttonValues(last)) != 0 {
		t.Error("a resolved quorum closes the message")
	}
	if text := allText(last); !strings.Contains(text, "<@U0ALICE>, <@U0BOB>") {
		t.Errorf("both approvers should be named: %q", text)
	}
}

func TestGateRefusalLeavesTheApprovalOpen(t *testing.T) {
	h := newHarness(t)
	h.gate.respond = func(n int) (int, string) {
		if n == 1 {
			return http.StatusForbidden, ``
		}
		return http.StatusOK, `{"status":"recorded","resolved":true}`
	}
	d := h.dispatch()
	h.post(d)

	h.click("U0ALICE", verbApprove, d.ApprovalID, "")
	if n := len(h.slack.byMethod("chat.update")); n != 0 {
		t.Fatal("a refused decision must not close the message")
	}
	if e := h.slack.byMethod("chat.postEphemeral"); len(e) != 1 || !strings.Contains(e[0].body["text"].(string), "refused") {
		t.Fatalf("the clicker should hear the refusal: %+v", e)
	}
	h.click("U0BOB", verbApprove, d.ApprovalID, "")
	if n := len(h.gate.all()); n != 2 {
		t.Fatalf("another approver may still decide; got %d decisions", n)
	}
	if n := len(h.slack.byMethod("chat.update")); n != 1 {
		t.Fatal("the accepted decision closes the message")
	}
}

func TestGateExpiredAndResolvedCloseTheMessage(t *testing.T) {
	for code, word := range map[int]string{http.StatusGone: "Expired", http.StatusConflict: "Already resolved"} {
		h := newHarness(t)
		h.gate.respond = func(int) (int, string) { return code, `{}` }
		d := h.dispatch()
		h.post(d)
		h.click("U0ALICE", verbApprove, d.ApprovalID, "")
		updates := h.slack.byMethod("chat.update")
		if len(updates) != 1 || len(buttonValues(blocksOf(updates[0]))) != 0 {
			t.Fatalf("HTTP %d: the message should close", code)
		}
		if !strings.Contains(allText(blocksOf(updates[0])), word) {
			t.Errorf("HTTP %d: outcome should say %q", code, word)
		}
	}
}

func TestExpiryClosesTheMessage(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	h.rs.expire(d.ApprovalID)

	updates := h.slack.byMethod("chat.update")
	if len(updates) != 1 || len(buttonValues(blocksOf(updates[0]))) != 0 {
		t.Fatal("an expired approval loses its buttons")
	}
	h.click("U0ALICE", verbApprove, d.ApprovalID, "")
	if n := len(h.gate.all()); n != 0 {
		t.Fatalf("a click after expiry must not be sent; got %d", n)
	}
}

func TestClickOnUnknownApproval(t *testing.T) {
	h := newHarness(t)
	h.click("U0ALICE", verbApprove, "approval-from-before-a-restart", "")
	if n := len(h.gate.all()); n != 0 {
		t.Fatal("nothing to decide")
	}
	if e := h.slack.byMethod("chat.postEphemeral"); len(e) != 1 || !strings.Contains(e[0].body["text"].(string), "no longer open") {
		t.Fatalf("want a 'no longer open' note, got %+v", e)
	}
}

func TestClickFromAnotherMessageIsIgnored(t *testing.T) {
	h := newHarness(t)
	d := h.dispatch()
	h.post(d)
	body := clickBody("U0ALICE", verbApprove, d.ApprovalID, "", "1699999999.000001")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	if code := h.sendInteraction(body, ts, slackSign([]byte(testSigningSecret), ts, body)); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	h.rs.wg.Wait()
	if n := len(h.gate.all()); n != 0 {
		t.Fatal("a click on a message this relay did not post must not decide")
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.relay.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
}
