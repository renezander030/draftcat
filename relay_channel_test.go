package main

// End-to-end tests for the relay OperatorChannel: a fake relay stands in for a
// Power Automate flow, dispatching decisions back at the real callback server.
//
// These are the tests that justify `relay` appearing in internal/channels: they
// exercise a complete round trip in the binary, not a stub.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/channels"
	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/relay"
)

const testRelaySecret = "conformance-secret"

// freePort grabs an unused local port so parallel tests do not collide.
func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// fakeRelay captures the dispatch and lets the test post decisions back.
type fakeRelay struct {
	srv *httptest.Server

	mu   sync.Mutex
	last relay.Request
	got  bool
	// sigOK records whether the dispatch carried a signature this relay could
	// verify — a relay author's first question.
	sigOK bool
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		sigErr := relay.VerifySignature(r.Header.Get(relay.SigHeader), body, []byte(testRelaySecret), relay.MaxSkewSeconds, time.Now())
		var req relay.Request
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.last, f.got, f.sigOK = req, true, sigErr == nil
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK) // ack immediately; never hold the human
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) dispatch(t *testing.T) relay.Request {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got, req := f.got, f.last
		f.mu.Unlock()
		if got {
			return req
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("relay never received a dispatch")
	return relay.Request{}
}

// postDecision signs and posts a decision at the channel's callback server,
// exactly as a real relay would.
func postDecision(t *testing.T, callbackURL string, d relay.Decision, secret string, at time.Time) *http.Response {
	t.Helper()
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, callbackURL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(relay.SigHeader, relay.Sign([]byte(secret), body, at))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("post decision: %v", err)
	}
	return resp
}

func newTestRelayChannel(t *testing.T, f *fakeRelay, ops []config.RelayOperator) (*RelayChannel, string) {
	t.Helper()
	addr := freePort(t)
	allowed := make([]int64, 0, len(ops))
	for _, o := range ops {
		allowed = append(allowed, o.ID)
	}
	cfg := config.RelayConfig{
		URL:          f.srv.URL,
		SecretEnv:    "DRAFTCAT_RELAY_SECRET",
		CallbackAddr: addr,
		PublicURL:    "http://" + addr,
		Operators:    ops,
		Security:     config.ChannelSecurity{AllowedUsers: allowed, MaxInputLength: 500, RateLimit: 10},
	}
	cfg.SetSecret(testRelaySecret)
	rc, err := NewRelayChannel(cfg)
	if err != nil {
		t.Fatalf("NewRelayChannel: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	callback := "http://" + addr + relay.CallbackPath
	// Wait for the callback server to accept connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var d net.Dialer
		d.Timeout = 100 * time.Millisecond
		if c, err := d.DialContext(context.Background(), "tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return rc, callback
}

var twoOps = []config.RelayOperator{
	{ID: 111, Identity: "alice@example.com"},
	{ID: 222, Identity: "bob@example.com"},
}

// RelayChannel must satisfy OperatorChannel — this is what earns `relay` a place
// in internal/channels.
var _ OperatorChannel = (*RelayChannel)(nil)

func TestRelayChannel_NameIsRelay(t *testing.T) {
	f := newFakeRelay(t)
	rc, _ := newTestRelayChannel(t, f, twoOps)
	if rc.Name() != channels.Relay {
		t.Fatalf("Name() = %q, want %q", rc.Name(), channels.Relay)
	}
	if !channels.IsImplemented(channels.Relay) {
		t.Fatal("relay is not registered as implemented")
	}
}

func TestRelayChannel_ApproveRoundTrip(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)

	ctx, cancel := context.WithTimeout(withStep(withRun(context.Background(), "run-1", "invoices"), "send-followup"), 5*time.Second)
	defer cancel()

	type res struct {
		d   OperatorDecision
		err error
	}
	out := make(chan res, 1)
	go func() {
		d, err := rc.SendForApproval(ctx, "Hi Anna, following up on invoice 2026-114.", nil)
		out <- res{d, err}
	}()

	req := f.dispatch(t)
	if !f.sigOK {
		t.Fatal("dispatch was not signed with a verifiable HMAC")
	}
	// The envelope must carry the run identity, or the audit trail cannot join.
	if req.RunID != "run-1" || req.Pipeline != "invoices" || req.Step != "send-followup" {
		t.Fatalf("envelope lost run context: run=%q pipeline=%q step=%q", req.RunID, req.Pipeline, req.Step)
	}
	if req.PayloadHash != relay.HashPayload("Hi Anna, following up on invoice 2026-114.") {
		t.Fatalf("payload hash does not cover the draft: %q", req.PayloadHash)
	}
	if req.Callback.Nonce == "" || req.Callback.URL != callback {
		t.Fatalf("callback not addressed correctly: %+v", req.Callback)
	}

	resp := postDecision(t, callback, relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionApprove, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "alice@example.com", Channel: "teams"},
	}, testRelaySecret, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback returned %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("SendForApproval: %v", r.err)
		}
		if r.d.Action != "approve" {
			t.Fatalf("action = %q, want approve", r.d.Action)
		}
		// The approver must come back as the internal operator id, so quorum
		// counting and the audit trail keep working unchanged.
		if r.d.ApproverID != 111 {
			t.Fatalf("ApproverID = %d, want 111", r.d.ApproverID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate never resolved after a valid approval")
	}
}

func TestRelayChannel_AdjustCarriesText(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make(chan OperatorDecision, 1)
	go func() {
		d, _ := rc.SendForApproval(ctx, "draft", nil)
		out <- d
	}()
	req := f.dispatch(t)
	resp := postDecision(t, callback, relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionAdjust, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "bob@example.com"}, AdjustText: "soften the second paragraph",
	}, testRelaySecret, time.Now())
	_ = resp.Body.Close()

	select {
	case d := <-out:
		if d.Action != "adjust" || d.Text != "soften the second paragraph" {
			t.Fatalf("got %+v, want adjust with text", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adjust never resolved the gate")
	}
}

// Quorum counts DISTINCT approvers. One person tapping twice must not satisfy a
// two-of-two gate.
func TestRelayChannel_QuorumNeedsDistinctApprovers(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make(chan QuorumDecision, 1)
	go func() {
		d, _ := rc.SendForQuorumApproval(ctx, "draft", 2, nil)
		out <- d
	}()
	req := f.dispatch(t)
	mk := func(id string) relay.Decision {
		return relay.Decision{
			Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
			Decision: relay.ActionApprove, PayloadHash: req.PayloadHash,
			Approver: relay.Approver{ID: id},
		}
	}

	r1 := postDecision(t, callback, mk("alice@example.com"), testRelaySecret, time.Now())
	_ = r1.Body.Close()
	// Same approver again — must not close a 2-of-2 gate.
	r2 := postDecision(t, callback, mk("alice@example.com"), testRelaySecret, time.Now())
	_ = r2.Body.Close()

	select {
	case d := <-out:
		t.Fatalf("gate resolved on a duplicate approver: %+v", d)
	case <-time.After(300 * time.Millisecond):
	}

	r3 := postDecision(t, callback, mk("bob@example.com"), testRelaySecret, time.Now())
	_ = r3.Body.Close()
	select {
	case d := <-out:
		if d.Action != "approve" || len(d.Approvers) != 2 {
			t.Fatalf("got %+v, want approve from 2 distinct approvers", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate never resolved after two distinct approvals")
	}
}

// The five security checks, over the wire against the real handler.
func TestRelayChannel_ForgedDecisionsAreRefused(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _, _ = rc.SendForApproval(ctx, "wire EUR 40,000 to DE99", nil) }()
	req := f.dispatch(t)

	valid := relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionApprove, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "alice@example.com"},
	}

	cases := []struct {
		name   string
		mutate func(d *relay.Decision)
		secret string
		at     time.Time
		want   int
	}{
		{"wrong secret", func(*relay.Decision) {}, "not-the-secret", time.Now(), http.StatusUnauthorized},
		{"stale timestamp", func(*relay.Decision) {}, testRelaySecret, time.Now().Add(-20 * time.Minute), http.StatusUnauthorized},
		{"wrong nonce", func(d *relay.Decision) { d.Nonce = "deadbeef" }, testRelaySecret, time.Now(), http.StatusForbidden},
		{"mutated payload hash", func(d *relay.Decision) { d.PayloadHash = relay.HashPayload("something else") }, testRelaySecret, time.Now(), http.StatusForbidden},
		{"unknown approver", func(d *relay.Decision) { d.Approver.ID = "mallory@example.com" }, testRelaySecret, time.Now(), http.StatusForbidden},
		{"relay-declared timeout", func(d *relay.Decision) { d.Decision = "timeout" }, testRelaySecret, time.Now(), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			tc.mutate(&d)
			resp := postDecision(t, callback, d, tc.secret, tc.at)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s: status %d, want %d — a forged decision was accepted", tc.name, resp.StatusCode, tc.want)
			}
		})
	}
}

// A decision for a gate that already closed is a 409, so a relay retry is safe.
func TestRelayChannel_ResolvedGateIsIdempotent(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { _, _ = rc.SendForApproval(ctx, "draft", nil); close(done) }()
	req := f.dispatch(t)
	d := relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionApprove, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "alice@example.com"},
	}
	r1 := postDecision(t, callback, d, testRelaySecret, time.Now())
	_ = r1.Body.Close()
	<-done

	r2 := postDecision(t, callback, d, testRelaySecret, time.Now())
	defer func() { _ = r2.Body.Close() }()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("retry status %d, want 409 so a relay can safely retry", r2.StatusCode)
	}
}

// A silent relay must produce a timeout, and a timeout must not fire the action.
func TestRelayChannel_SilentRelayTimesOut(t *testing.T) {
	f := newFakeRelay(t)
	rc, _ := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	d, err := rc.SendForApproval(ctx, "draft nobody answers", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != "timeout" {
		t.Fatalf("action = %q, want timeout when the relay never answers", d.Action)
	}
}

// A step's approver scope must reach the wire and be enforced on the callback.
func TestRelayChannel_StepApproverScopeIsEnforced(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Only bob (222) may decide this step.
	go func() { _, _ = rc.SendForApproval(ctx, "draft", []int64{222}) }()
	req := f.dispatch(t)
	if len(req.Approvers) != 1 || req.Approvers[0] != "bob@example.com" {
		t.Fatalf("scoped approvers not on the wire: %v", req.Approvers)
	}
	// Alice is an allowed operator on the channel but out of scope for the step.
	resp := postDecision(t, callback, relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionApprove, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "alice@example.com"},
	}, testRelaySecret, time.Now())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — an out-of-scope approver decided a scoped step", resp.StatusCode)
	}
}

// An unsatisfiable gate must fail fast rather than hang to timeout.
func TestRelayChannel_UnsatisfiableQuorumFailsFast(t *testing.T) {
	f := newFakeRelay(t)
	rc, _ := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := rc.SendForQuorumApproval(ctx, "draft", 3, nil)
	if err == nil {
		t.Fatal("a quorum larger than the operator pool was accepted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("unsatisfiable quorum hung instead of failing fast")
	}
}
