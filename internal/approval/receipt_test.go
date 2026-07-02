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
