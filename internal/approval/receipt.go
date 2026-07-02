// Package approval turns draftcat's approval audit rows into tamper-evident
// receipts.
//
// draftcat already records who approved which payload-hash when (the
// action_approvals table, append-only by convention). That convention protects
// against draftcat's own code path, but not against anyone who can touch the
// SQLite file directly. Signing each row with an HMAC over its canonical fields
// closes that gap: you can later prove "operator X approved payload H at time T"
// without trusting the database, and any post-hoc edit — swapping the approver,
// the decision, or the payload hash — breaks the signature. This is what a
// governed pipeline needs to stand behind an approval under audit.
//
// The secret is symmetric (HMAC), so a verifier must hold the same key that
// signed. That fits draftcat's one-business-per-instance model: the operator who
// runs the instance holds the key and can prove integrity to a third party by
// re-signing, or by handing over the key for an audit.
package approval

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Fields are the immutable facts of one approval decision — exactly the columns
// persisted in action_approvals. The signature covers all of them plus a nonce,
// so altering any single one invalidates the receipt.
type Fields struct {
	Pipeline    string
	Step        string
	DecidedAt   int64 // unix seconds
	Decision    string
	OperatorID  int64
	PayloadHash string // sha256 hex of the exact draft shown
	QuorumN     int
	QuorumGot   int
}

// canonical serializes (fields, nonce) into an unambiguous byte string. Each
// value is length-prefixed so no combination of field values can collide with a
// different set of fields — e.g. pipeline="a", step="b|c" must not sign the same
// bytes as pipeline="a|b", step="c". Without length prefixing a naive "a|b|c"
// join would let an attacker shift a delimiter and preserve the signature.
func canonical(f Fields, nonce string) []byte {
	parts := []string{
		f.Pipeline,
		f.Step,
		strconv.FormatInt(f.DecidedAt, 10),
		f.Decision,
		strconv.FormatInt(f.OperatorID, 10),
		f.PayloadHash,
		strconv.Itoa(f.QuorumN),
		strconv.Itoa(f.QuorumGot),
		nonce,
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte('|')
	}
	return []byte(b.String())
}

// NewNonce returns a random 128-bit nonce (hex). A per-row nonce means two
// otherwise-identical decisions still produce distinct receipts, and it makes a
// signed row impossible to replay onto a different row.
func NewNonce() (string, error) {
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", fmt.Errorf("approval: generate nonce: %w", err)
	}
	return hex.EncodeToString(n[:]), nil
}

// Sign returns the hex HMAC-SHA256 of the canonical (fields, nonce) under secret.
func Sign(secret []byte, f Fields, nonce string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(canonical(f, nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is a valid receipt for (fields, nonce) under secret,
// using a constant-time comparison so a caller can't learn the correct signature
// byte-by-byte through timing. A false result means the row was altered, signed
// with a different key, or never signed.
func Verify(secret []byte, f Fields, nonce, sig string) bool {
	want := Sign(secret, f, nonce)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}
