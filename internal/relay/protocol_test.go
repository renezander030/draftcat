package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("relay-shared-secret")

func mustNonce(t *testing.T) string {
	t.Helper()
	n, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	return n
}

// fixture builds a matched (Decision, Pending) pair that VerifyDecision accepts,
// so each test can break exactly one thing and assert that check fires.
func fixture(t *testing.T) (*Decision, *Pending, time.Time) {
	t.Helper()
	now := time.Unix(1785661262, 0)
	nonce := mustNonce(t)
	hash := HashPayload("Hi Anna, following up on invoice 2026-114.")
	d := &Decision{
		Protocol:    Version,
		ApprovalID:  "01J8Z7ABC",
		Nonce:       nonce,
		Decision:    ActionApprove,
		PayloadHash: hash,
		Approver:    Approver{ID: "alice@example.com", Display: "Alice Reuter", Channel: "teams"},
		DecidedAt:   now.UTC().Format(time.RFC3339),
	}
	p := &Pending{
		ApprovalID:  "01J8Z7ABC",
		Nonce:       nonce,
		PayloadHash: hash,
		Permitted:   map[string]bool{"alice@example.com": true, "bob@example.com": true},
		ExpiresAt:   now.Add(4 * time.Hour),
	}
	return d, p, now
}

func TestVerifyDecision_HappyPath(t *testing.T) {
	d, p, now := fixture(t)
	if err := VerifyDecision(d, p, now); err != nil {
		t.Fatalf("well-formed decision rejected: %v", err)
	}
}

// The payload-hash echo is the check that makes "the human approved THIS text"
// verifiable. A relay that renders a different draft than the gate staged must
// not be able to produce an approval for it.
func TestVerifyDecision_MutatedPayloadHashRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.PayloadHash = HashPayload("Hi Anna, wire EUR 40,000 to account DE99.")
	err := VerifyDecision(d, p, now)
	if err == nil {
		t.Fatal("a decision echoing a different payload hash was accepted")
	}
	if !strings.Contains(err.Error(), "payload_hash mismatch") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestVerifyDecision_NonceMismatchRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Nonce = mustNonce(t)
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("a decision carrying the wrong nonce was accepted")
	}
}

// A relay reports who decided. It must not be able to invent an approver or
// widen the permitted set beyond the step's scope.
func TestVerifyDecision_UnpermittedApproverRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Approver.ID = "mallory@example.com"
	err := VerifyDecision(d, p, now)
	if err == nil {
		t.Fatal("a decision from an out-of-scope approver was accepted")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestVerifyDecision_MissingApproverRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Approver.ID = ""
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("a decision with no approver identity was accepted")
	}
}

func TestVerifyDecision_ExpiredGateRejected(t *testing.T) {
	d, p, now := fixture(t)
	if err := VerifyDecision(d, p, now.Add(5*time.Hour)); err == nil {
		t.Fatal("a decision arriving after expiry was accepted")
	}
}

func TestVerifyDecision_WrongApprovalIDRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.ApprovalID = "01J8Z7OTHER"
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("a decision for a different approval was accepted")
	}
}

// "timeout" is the gate's call on the gate's clock. A relay that could declare
// one could starve a gate into it.
func TestVerifyDecision_TimeoutFromRelayRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Decision = "timeout"
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("a relay-declared timeout was accepted as a decision")
	}
}

func TestVerifyDecision_UnknownVerbRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Decision = "execute"
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("an unknown decision verb was accepted")
	}
}

func TestVerifyDecision_WrongProtocolRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Protocol = "hitl/v1"
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("a decision from a different protocol version was accepted")
	}
}

func TestVerifyDecision_EmptyAdjustTextRejected(t *testing.T) {
	d, p, now := fixture(t)
	d.Decision = ActionAdjust
	d.AdjustText = "   "
	if err := VerifyDecision(d, p, now); err == nil {
		t.Fatal("an adjust decision with blank text was accepted")
	}
}

// An empty Permitted set means the step did not narrow the approver list; the
// channel's own allowed-user check still applies upstream.
func TestVerifyDecision_EmptyPermittedDoesNotBlock(t *testing.T) {
	d, p, now := fixture(t)
	p.Permitted = nil
	if err := VerifyDecision(d, p, now); err != nil {
		t.Fatalf("unscoped step rejected a valid approver: %v", err)
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	now := time.Unix(1785661262, 0)
	body := []byte(`{"protocol":"hitl/v0","decision":"approve"}`)
	if err := VerifySignature(Sign(testSecret, body, now), body, testSecret, MaxSkewSeconds, now); err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestVerifySignature_TamperedBodyFails(t *testing.T) {
	now := time.Unix(1785661262, 0)
	body := []byte(`{"decision":"skip"}`)
	sig := Sign(testSecret, body, now)
	if err := VerifySignature(sig, []byte(`{"decision":"approve"}`), testSecret, MaxSkewSeconds, now); err == nil {
		t.Fatal("a signature was accepted over a body it did not sign")
	}
}

func TestVerifySignature_WrongSecretFails(t *testing.T) {
	now := time.Unix(1785661262, 0)
	body := []byte(`{"decision":"approve"}`)
	sig := Sign([]byte("other-secret"), body, now)
	if err := VerifySignature(sig, body, testSecret, MaxSkewSeconds, now); err == nil {
		t.Fatal("a signature made with the wrong secret was accepted")
	}
}

// Bounding skew is what stops a captured request from replaying forever.
func TestVerifySignature_StaleAndFutureTimestampsFail(t *testing.T) {
	now := time.Unix(1785661262, 0)
	body := []byte(`{"decision":"approve"}`)
	sig := Sign(testSecret, body, now)
	if err := VerifySignature(sig, body, testSecret, MaxSkewSeconds, now.Add(10*time.Minute)); err == nil {
		t.Fatal("a stale signature was accepted")
	}
	if err := VerifySignature(sig, body, testSecret, MaxSkewSeconds, now.Add(-10*time.Minute)); err == nil {
		t.Fatal("a future-dated signature was accepted")
	}
}

func TestVerifySignature_MissingAndMalformedHeadersFail(t *testing.T) {
	now := time.Unix(1785661262, 0)
	body := []byte(`{}`)
	for _, h := range []string{"", "garbage", "t=1785661262", "v1=abcdef", "t=notanumber,v1=abcdef"} {
		if err := VerifySignature(h, body, testSecret, MaxSkewSeconds, now); err == nil {
			t.Fatalf("malformed header %q was accepted", h)
		}
	}
}

func TestDecodeDecision_RejectsOversizedBody(t *testing.T) {
	big := make([]byte, MaxDecisionSize+1)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := DecodeDecision(big); err == nil {
		t.Fatal("an oversized decision body was decoded")
	}
}

// Unknown fields must be ignored so the gate can add context without breaking
// relays already deployed in the field.
func TestDecodeDecision_IgnoresUnknownFields(t *testing.T) {
	body := []byte(`{"protocol":"hitl/v0","approval_id":"a","decision":"approve","future_field":{"x":1}}`)
	d, err := DecodeDecision(body)
	if err != nil {
		t.Fatalf("decode with unknown field failed: %v", err)
	}
	if d.Decision != ActionApprove {
		t.Fatalf("decision = %q, want approve", d.Decision)
	}
}

func TestHashPayload_IsStableAndDistinguishing(t *testing.T) {
	a := HashPayload("draft one")
	if a != HashPayload("draft one") {
		t.Fatal("payload hash is not stable")
	}
	if a == HashPayload("draft two") {
		t.Fatal("payload hash does not distinguish drafts")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("payload hash %q lacks the sha256: prefix", a)
	}
}

func TestNewNonce_IsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		n := mustNonce(t)
		if seen[n] {
			t.Fatal("NewNonce returned a duplicate")
		}
		seen[n] = true
	}
}

// The dispatch envelope must survive a JSON round trip unchanged — a relay
// author reads these field names off the wire.
func TestRequest_JSONRoundtrip(t *testing.T) {
	req := Request{
		Protocol: Version, ApprovalID: "a1", RunID: "r1",
		Pipeline: "invoice-due-diligence", Step: "send-followup",
		Quorum:      Quorum{Required: 2},
		Approvers:   []string{"alice@example.com"},
		PayloadHash: HashPayload("body"),
		Draft:       Draft{ContentType: "text/plain", Body: "body"},
		Budget:      &Budget{Unit: "EUR", SpentToday: 4.12, CapToday: 20},
		Actions:     []Action{{Verb: ActionApprove}, {Verb: ActionAdjust, Accepts: "text"}},
		Callback:    Callback{URL: "https://gate.example.com" + CallbackPath, Nonce: "n1"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Request
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PayloadHash != req.PayloadHash || back.Quorum.Required != 2 || back.Budget.CapToday != 20 {
		t.Fatalf("round trip lost fields: %+v", back)
	}
	if !strings.Contains(string(raw), `"run_id"`) {
		t.Fatal("run_id missing from the wire form")
	}
}
