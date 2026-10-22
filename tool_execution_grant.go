package main

// Preconditions and execution grants: what an allowed permit says about the
// moment it is used.
//
// A precondition is the state of the target the human saw (a version, an
// ETag, a content digest). It is bound into the permit, and the permit is
// consumed only when the harness reports the same state, so an approval of a
// change to record v7 is not spent on v8.
//
// An execution grant is the gate's signed statement that one permit was
// consumed for one exact call. The tool checks it with the shared key before
// acting, so a call that never passed the gate carries no grant the tool will
// accept.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	statestore "github.com/renezander030/draftcat/internal/state"
)

const maxPreconditionLen = 512

func validPrecondition(p string) bool {
	if len(p) > maxPreconditionLen {
		return false
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// preconditionHolds reports whether the target state the harness reports at
// consume matches the one bound into the permit. A permit bound without a
// precondition needs none.
func preconditionHolds(a statestore.ToolAction, reported string) (bool, string) {
	unbound := toolBindingHash(a.ActionID, a.Agent, a.Tool, a.RunID, a.ArgsHash, a.PolicyHash, a.ExpiresAt, "")
	if unbound == a.BindingHash {
		return true, ""
	}
	if reported == "" {
		return false, "this permit is bound to a target precondition; send the target's current state as precondition"
	}
	if toolBindingHash(a.ActionID, a.Agent, a.Tool, a.RunID, a.ArgsHash, a.PolicyHash, a.ExpiresAt, reported) != a.BindingHash {
		return false, "the target changed since the decision; request a new approval"
	}
	return true, ""
}

const executionGrantPrefix = "dcg1."

// executionGrant is the signed payload of a grant.
type executionGrant struct {
	ActionID    string `json:"action_id"`
	Tool        string `json:"tool"`
	Agent       string `json:"agent,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	ArgsHash    string `json:"args_hash"`
	BindingHash string `json:"binding_hash"`
	ConsumedAt  string `json:"consumed_at"`
	ExpiresAt   string `json:"expires_at"`
}

func executionGrantFor(a statestore.ToolAction, consumedAt time.Time) executionGrant {
	return executionGrant{
		ActionID: a.ActionID, Tool: a.Tool, Agent: a.Agent, RunID: a.RunID,
		ArgsHash: a.ArgsHash, BindingHash: a.BindingHash,
		ConsumedAt: consumedAt.UTC().Format(time.RFC3339),
		ExpiresAt:  a.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// signExecutionGrant renders dcg1.<payload>.<mac>: the payload is base64url
// JSON, the mac is HMAC-SHA256 over "dcg1.<payload>" with the shared key.
func signExecutionGrant(key []byte, g executionGrant) string {
	b, _ := json.Marshal(g)
	body := executionGrantPrefix + base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyExecutionGrant checks a grant's signature, that it covers this tool
// and these exact arguments, and that it was issued no longer than maxAge ago
// (0 = no age limit).
func verifyExecutionGrant(key []byte, grant, tool string, args map[string]interface{}, maxAge time.Duration, now time.Time) (executionGrant, error) {
	var g executionGrant
	if len(key) == 0 {
		return g, errors.New("no grant key")
	}
	if !strings.HasPrefix(grant, executionGrantPrefix) {
		return g, errors.New("not a draftcat execution grant")
	}
	i := strings.LastIndexByte(grant, '.')
	if i <= len(executionGrantPrefix) {
		return g, errors.New("malformed grant")
	}
	body, sig := grant[:i], grant[i+1:]
	want := hmac.New(sha256.New, key)
	want.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return g, errors.New("signature does not verify")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(body, executionGrantPrefix))
	if err != nil || json.Unmarshal(raw, &g) != nil {
		return g, errors.New("malformed grant payload")
	}
	if g.Tool != tool {
		return g, fmt.Errorf("grant is for tool %q, not %q", g.Tool, tool)
	}
	if h := hashToolArgs(args); g.ArgsHash != h {
		return g, fmt.Errorf("grant covers arguments %s, not %s", g.ArgsHash, h)
	}
	at, err := time.Parse(time.RFC3339, g.ConsumedAt)
	if err != nil {
		return g, errors.New("malformed grant time")
	}
	if maxAge > 0 && now.Sub(at) > maxAge {
		return g, fmt.Errorf("grant issued %s ago, older than %s", now.Sub(at).Round(time.Second), maxAge)
	}
	return g, nil
}
