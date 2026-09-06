package zkreceipt

import (
	"strings"
	"testing"
)

var validRecord = Record{
	Pipeline:    "customer-refund",
	Step:        "send-refund",
	DecidedAt:   1788580800,
	Decision:    "approve",
	OperatorID:  4815162342,
	PayloadHash: "1b4a1f4e20d8228d0df23d2b4b2ac302adca14c311f5210c1092cc11890a4f8e",
	QuorumN:     2,
	QuorumGot:   3,
	Nonce:       "61f6ba5968994b80a1fa9e360fb173f1",
}

var testSecret = []byte("correct horse battery staple plus entropy")

func TestProveAndVerify(t *testing.T) {
	bundle, err := Prove(validRecord, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ProofBytes == 0 || bundle.KeyCommitment == "" || bundle.ReceiptCommitment == "" {
		t.Fatalf("incomplete bundle: %+v", bundle)
	}
	expectedKey, err := KeyCommitment(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(bundle, expectedKey); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	encoded, err := MarshalBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{validRecord.Pipeline, validRecord.Step, validRecord.PayloadHash, validRecord.Nonce} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatalf("proof bundle disclosed private value %q", privateValue)
		}
	}
}

func TestVerifyRejectsTamperedCommitment(t *testing.T) {
	bundle, err := Prove(validRecord, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	bundle.ReceiptCommitment = strings.Repeat("0", 64)
	if err := Verify(bundle, bundle.KeyCommitment); err == nil {
		t.Fatal("tampered public commitment was accepted")
	}
}

func TestVerifyRejectsWrongPinnedKey(t *testing.T) {
	bundle, err := Prove(validRecord, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := KeyCommitment([]byte("different secret with thirty two plus bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(bundle, wrongKey); err == nil {
		t.Fatal("proof was accepted under the wrong pinned key")
	}
}

func TestProveRejectsNonHumanAndUnderQuorum(t *testing.T) {
	policy := validRecord
	policy.Decision = "policy_approve"
	if _, err := Prove(policy, testSecret); err == nil {
		t.Fatal("automated policy approval was accepted")
	}
	underQuorum := validRecord
	underQuorum.QuorumGot = 1
	if _, err := Prove(underQuorum, testSecret); err == nil {
		t.Fatal("under-quorum approval was accepted")
	}
}

func TestKeyCommitmentChangesWithSecret(t *testing.T) {
	one, err := KeyCommitment(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	two, err := KeyCommitment([]byte("another secret with more than enough random bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("different instance secrets produced the same commitment")
	}
}
