package approval

import "testing"

func sampleFields() Fields {
	return Fields{
		Pipeline:    "sales-inbox",
		Step:        "draft-reply",
		DecidedAt:   1_750_000_000,
		Decision:    "approve",
		OperatorID:  42,
		PayloadHash: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		QuorumN:     2,
		QuorumGot:   2,
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	secret := []byte("test-secret")
	f := sampleFields()
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	sig := Sign(secret, f, nonce)
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !Verify(secret, f, nonce, sig) {
		t.Fatal("valid receipt failed to verify")
	}
}

// Tampering with ANY field must invalidate the receipt — that is the whole point
// of a signed audit row. Mutate each field in turn and assert verification fails.
func TestTamperFailsPerField(t *testing.T) {
	secret := []byte("test-secret")
	f := sampleFields()
	nonce, _ := NewNonce()
	sig := Sign(secret, f, nonce)

	mutations := map[string]func(*Fields){
		"pipeline":     func(x *Fields) { x.Pipeline = "other" },
		"step":         func(x *Fields) { x.Step = "other" },
		"decided_at":   func(x *Fields) { x.DecidedAt++ },
		"decision":     func(x *Fields) { x.Decision = "skip" },
		"operator_id":  func(x *Fields) { x.OperatorID = 43 },
		"payload_hash": func(x *Fields) { x.PayloadHash = "deadbeef" },
		"quorum_n":     func(x *Fields) { x.QuorumN = 1 },
		"quorum_got":   func(x *Fields) { x.QuorumGot = 1 },
	}
	for name, mutate := range mutations {
		tampered := f
		mutate(&tampered)
		if Verify(secret, tampered, nonce, sig) {
			t.Errorf("verification passed after tampering with %q — receipt is not binding", name)
		}
	}
}

func TestWrongSecretFails(t *testing.T) {
	f := sampleFields()
	nonce, _ := NewNonce()
	sig := Sign([]byte("real-secret"), f, nonce)
	if Verify([]byte("attacker-secret"), f, nonce, sig) {
		t.Fatal("receipt verified under the wrong secret")
	}
}

func TestWrongNonceFails(t *testing.T) {
	secret := []byte("test-secret")
	f := sampleFields()
	n1, _ := NewNonce()
	n2, _ := NewNonce()
	sig := Sign(secret, f, n1)
	if Verify(secret, f, n2, sig) {
		t.Fatal("receipt verified with a different nonce — replay protection broken")
	}
}

// The length-prefixed canonical form must stop delimiter-shift collisions:
// moving a "|" between two adjacent fields must not preserve the signature.
func TestCanonicalNoDelimiterCollision(t *testing.T) {
	secret := []byte("test-secret")
	nonce := "fixed-nonce"
	a := sampleFields()
	a.Pipeline, a.Step = "a", "b|c"
	b := sampleFields()
	b.Pipeline, b.Step = "a|b", "c"
	if Sign(secret, a, nonce) == Sign(secret, b, nonce) {
		t.Fatal("delimiter-shifted fields produced the same signature")
	}
}

func TestNonceIsRandom(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		n, err := NewNonce()
		if err != nil {
			t.Fatalf("NewNonce: %v", err)
		}
		if seen[n] {
			t.Fatalf("duplicate nonce %q", n)
		}
		seen[n] = true
	}
}

func TestEmptySignatureFails(t *testing.T) {
	if Verify([]byte("s"), sampleFields(), "n", "") {
		t.Fatal("empty signature verified")
	}
}

func TestV2BindsActionPolicyAndExpiry(t *testing.T) {
	secret := []byte("test-secret")
	f := FieldsV2{
		ReceiptID: "rcpt_1", RunID: "run_1", ActionID: "send_1",
		Pipeline: "sales", Step: "send", DecidedAt: 1_750_000_000,
		Decision: "approve", OperatorID: 42, PayloadHash: "sha256:payload",
		Policy: "human-approval", PolicyHash: "sha256:policy",
		BindingHash: "sha256:binding", ExpiresAt: 1_750_003_600,
		QuorumN: 1, QuorumGot: 1,
	}
	nonce := "fixed"
	sig := SignV2(secret, f, nonce)
	if !VerifyV2(secret, f, nonce, sig) {
		t.Fatal("valid v2 receipt did not verify")
	}
	for name, mutate := range map[string]func(*FieldsV2){
		"action":  func(x *FieldsV2) { x.ActionID = "send_2" },
		"policy":  func(x *FieldsV2) { x.PolicyHash = "sha256:changed" },
		"binding": func(x *FieldsV2) { x.BindingHash = "sha256:changed" },
		"expiry":  func(x *FieldsV2) { x.ExpiresAt++ },
	} {
		changed := f
		mutate(&changed)
		if VerifyV2(secret, changed, nonce, sig) {
			t.Errorf("v2 receipt still verified after %s drift", name)
		}
	}
}
