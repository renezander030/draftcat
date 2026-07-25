package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

// The inbound webhook STARTS a pipeline, so an attacker who can replay or
// tamper with a request can make draftcat act. A bearer token alone proves only
// that the caller once saw the token; these tests pin the body-and-time binding
// that closes that.

// newTempStateStore opens a throwaway SQLite store for tests that need the
// replay guard (which lives in seen_items).
func newTempStateStore(t *testing.T) *statestore.StateStore {
	t.Helper()
	s, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open temp state store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func signBody(secret []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhookSignature_ValidSignaturePasses(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"lead":"acme"}`)
	now := time.Now()

	err := verifyWebhookSignature(signBody(secret, now.Unix(), body), body, secret, 300, now)
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

// The whole point of signing the body: a swapped payload must not verify.
func TestVerifyWebhookSignature_TamperedBodyFails(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Now()
	header := signBody(secret, now.Unix(), []byte(`{"amount":10}`))

	err := verifyWebhookSignature(header, []byte(`{"amount":100000}`), secret, 300, now)
	if err == nil {
		t.Fatal("a body swapped after signing must fail verification")
	}
}

func TestVerifyWebhookSignature_WrongSecretFails(t *testing.T) {
	now := time.Now()
	body := []byte("x")
	header := signBody([]byte("attacker-guess"), now.Unix(), body)

	if err := verifyWebhookSignature(header, body, []byte("real-secret"), 300, now); err == nil {
		t.Fatal("signature made with the wrong secret must fail")
	}
}

// Bounding the clock skew is what stops an old captured request being re-fired
// indefinitely.
func TestVerifyWebhookSignature_StaleTimestampFails(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte("x")
	now := time.Now()
	old := now.Add(-30 * time.Minute).Unix()

	err := verifyWebhookSignature(signBody(secret, old, body), body, secret, 300, now)
	if err == nil {
		t.Fatal("a 30-minute-old signature must fail a 5-minute window")
	}
	if !strings.Contains(err.Error(), "window") {
		t.Errorf("error %q should explain the skew window", err)
	}
}

// A clock ahead of ours is equally suspect — the window is two-sided.
func TestVerifyWebhookSignature_FutureTimestampFails(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte("x")
	now := time.Now()
	future := now.Add(30 * time.Minute).Unix()

	if err := verifyWebhookSignature(signBody(secret, future, body), body, secret, 300, now); err == nil {
		t.Fatal("a far-future signature must fail the skew window")
	}
}

func TestVerifyWebhookSignature_MissingHeaderFails(t *testing.T) {
	if err := verifyWebhookSignature("", []byte("x"), []byte("s"), 300, time.Now()); err == nil {
		t.Fatal("absent signature header must fail when a signature is demanded")
	}
}

func TestVerifyWebhookSignature_MalformedHeaderFails(t *testing.T) {
	now := time.Now()
	for _, h := range []string{
		"garbage",
		"t=123",               // no v1
		"v1=abc",              // no t
		"t=notanumber,v1=abc", // unparseable timestamp
		fmt.Sprintf("t=%d", now.Unix()),
	} {
		if err := verifyWebhookSignature(h, []byte("x"), []byte("s"), 300, now); err == nil {
			t.Errorf("malformed header %q must fail", h)
		}
	}
}

// A signature is spent once. Within the skew window a valid signature would
// otherwise stay replayable, so the second presentation must be refused.
// (Uses the package-level state store, which the handler also consults.)
func TestVerifyWebhookSignature_ReplayIsRejected(t *testing.T) {
	prev := state
	state = newTempStateStore(t)
	t.Cleanup(func() { state = prev })

	secret := []byte("s3cret")
	body := []byte(`{"a":1}`)
	now := time.Now()
	header := signBody(secret, now.Unix(), body)

	if err := verifyWebhookSignature(header, body, secret, 300, now); err != nil {
		t.Fatalf("first use of a valid signature must pass: %v", err)
	}
	err := verifyWebhookSignature(header, body, secret, 300, now)
	if err == nil {
		t.Fatal("replaying the same signature must be rejected")
	}
	if !strings.Contains(err.Error(), "replay") {
		t.Errorf("error %q should name the replay", err)
	}
}

// Without a state store the signature still authenticates — we just cannot
// promise single-use. Verify that path does not error.
func TestVerifyWebhookSignature_NoStateStoreStillVerifies(t *testing.T) {
	prev := state
	state = nil
	t.Cleanup(func() { state = prev })

	secret := []byte("s3cret")
	body := []byte("x")
	now := time.Now()

	if err := verifyWebhookSignature(signBody(secret, now.Unix(), body), body, secret, 300, now); err != nil {
		t.Fatalf("signature check must work without a state store: %v", err)
	}
}
