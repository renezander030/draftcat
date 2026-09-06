package fhevote

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncryptedVotesAggregateWithoutReadableVotes(t *testing.T) {
	secret, public, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	context := "invoice-4821"
	ballotTokens := []string{"random-invite-alice", "random-invite-bob", "random-invite-carol"}
	votes := []bool{true, false, true}
	ballots := make([]Ballot, 0, len(votes))
	for i, vote := range votes {
		ballot, err := EncryptVote(public, context, ballotTokens[i], vote)
		if err != nil {
			t.Fatalf("encrypt vote %d: %v", i, err)
		}
		encoded, err := Marshal(ballot)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{context, ballotTokens[i], "approve", "reject"} {
			if strings.Contains(string(encoded), private) {
				t.Fatalf("encrypted ballot disclosed %q", private)
			}
		}
		ballots = append(ballots, ballot)
	}

	tally, err := Aggregate(public, context, ballots)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Decrypt(secret, context, tally)
	if err != nil {
		t.Fatal(err)
	}
	if result.Approvals != 2 || result.Ballots != 3 {
		t.Fatalf("unexpected tally: %+v", result)
	}
}

func TestAggregateRejectsWrongContextKeyAndDuplicateBallot(t *testing.T) {
	_, public, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPublic, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	ballot, err := EncryptVote(public, "release-7", "invite-1", true)
	if err != nil {
		t.Fatal(err)
	}
	ballot2, err := EncryptVote(public, "release-7", "invite-2", false)
	if err != nil {
		t.Fatal(err)
	}
	ballot3, err := EncryptVote(public, "release-7", "invite-3", true)
	if err != nil {
		t.Fatal(err)
	}
	ballots := []Ballot{ballot, ballot2, ballot3}
	if _, err := Aggregate(public, "release-8", ballots); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("wanted context rejection, got %v", err)
	}
	if _, err := Aggregate(otherPublic, "release-7", ballots); err == nil || !strings.Contains(err.Error(), "key ID") {
		t.Fatalf("wanted key rejection, got %v", err)
	}
	if _, err := Aggregate(public, "release-7", []Ballot{ballot, ballot, ballot3}); err == nil || !strings.Contains(err.Error(), "repeats ballot commitment") {
		t.Fatalf("wanted duplicate rejection, got %v", err)
	}
	if _, err := Aggregate(public, "release-7", []Ballot{ballot, ballot2}); err == nil || !strings.Contains(err.Error(), "at least 3") {
		t.Fatalf("wanted small-group rejection, got %v", err)
	}
}

func TestDecryptRejectsWrongContextAndMalformedTally(t *testing.T) {
	secret, public, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	ballot, err := EncryptVote(public, "payment-9", "invite-9", true)
	if err != nil {
		t.Fatal(err)
	}
	ballot2, err := EncryptVote(public, "payment-9", "invite-10", false)
	if err != nil {
		t.Fatal(err)
	}
	ballot3, err := EncryptVote(public, "payment-9", "invite-11", true)
	if err != nil {
		t.Fatal(err)
	}
	tally, err := Aggregate(public, "payment-9", []Ballot{ballot, ballot2, ballot3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(secret, "payment-10", tally); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("wanted context rejection, got %v", err)
	}

	badLength := tally
	badLength.CiphertextBytes++
	if _, err := Decrypt(secret, "payment-9", badLength); err == nil || !strings.Contains(err.Error(), "byte count") {
		t.Fatalf("wanted byte-count rejection, got %v", err)
	}

	missingContributor := tally
	missingContributor.Contributors = missingContributor.Contributors[:2]
	if _, err := Decrypt(secret, "payment-9", missingContributor); err == nil || !strings.Contains(err.Error(), "ballot count") {
		t.Fatalf("wanted contributor-count rejection, got %v", err)
	}

	wrongDeclaredCount := tally
	wrongDeclaredCount.Ballots++
	wrongDeclaredCount.Contributors = append(wrongDeclaredCount.Contributors, strings.Repeat("0", 64))
	if _, err := Decrypt(secret, "payment-9", wrongDeclaredCount); err == nil || !strings.Contains(err.Error(), "encrypted ballot count") {
		t.Fatalf("wanted encrypted-count rejection, got %v", err)
	}

	badEncoding := tally
	decoded, err := base64.StdEncoding.DecodeString(badEncoding.CiphertextBase64)
	if err != nil {
		t.Fatal(err)
	}
	decoded[len(decoded)/2] ^= 1
	badEncoding.CiphertextBase64 = base64.StdEncoding.EncodeToString(decoded)
	if _, err := Decrypt(secret, "payment-9", badEncoding); err == nil {
		t.Fatal("wanted altered ciphertext to fail decoding or aggregate validation")
	}
}

func TestStrictJSONRejectsUnknownAndMultipleValues(t *testing.T) {
	if _, err := ParseBallot([]byte(`{"schema":"x","extra":true}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("wanted unknown-field rejection, got %v", err)
	}
	if _, err := ParseTally([]byte(`{} {}`)); err == nil || !strings.Contains(err.Error(), "multiple values") {
		t.Fatalf("wanted multiple-value rejection, got %v", err)
	}
}
