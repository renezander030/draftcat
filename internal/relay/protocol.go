// Package relay implements the hitl/v0 wire protocol: the vendor-neutral
// contract between draftcat (the GATE, which owns policy, quorum, expiry, the
// payload hash and the audit trail) and a RELAY (any component that can show a
// draft to a human and post one decision back).
//
// The point of the split is that draftcat never takes ownership of a vendor's
// bot lifecycle. Microsoft Teams is the motivating case: incoming webhooks and
// O365 connectors were disabled in May 2026, Graph chatMessage cannot receive a
// card submit, the Graph Approvals API is still beta, and the Python Bot
// Framework SDK is archived — so the only in-binary path left is a registered
// bot with an Azure app registration and tenant admin consent, per vendor,
// forever. A relay moves that work into a component the operator's own tenant
// already trusts (a Power Automate flow, an existing bot, n8n, a shell script)
// and leaves draftcat with one channel to implement.
//
// The relay is UNTRUSTED. It renders the draft and reports who decided; it is
// trusted with nothing else. See VerifyDecision for the checks that let a
// compromised relay DENY (drop dispatches, stay silent — the gate times out and
// the action does not fire, which is the safe direction) but never AUTHORISE.
//
// See docs/hitl-protocol.md for the specification this file implements.
package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is the protocol string carried in every envelope. A breaking change
// bumps this and the callback path segment together (hitl/v1, /hitl/v1/...).
const Version = "hitl/v0"

// CallbackPath is where the gate listens for decision envelopes.
const CallbackPath = "/hitl/v0/decisions"

// SigHeader is deliberately the same header and the same
// `t=<unix>,v1=<hex hmac-sha256(t + "." + body)>` construction the inbound
// webhook trigger already uses. One signing scheme in the binary means one
// thing to get right, one thing to test, and one thing for a relay author to
// implement.
const (
	SigHeader       = "X-Draftcat-Signature"
	ProtocolHeader  = "X-Hitl-Protocol"
	MaxSkewSeconds  = 300
	MaxDecisionSize = 1 << 16
)

// Action verbs. Closed set in v0.
const (
	ActionApprove = "approve"
	ActionSkip    = "skip"
	ActionAdjust  = "adjust"
)

// Request is the dispatch envelope: gate -> relay.
//
// Unknown fields must be ignored by relays, so the gate can add context (a new
// budget key, a new risk level) without breaking deployments already in the
// field.
type Request struct {
	Protocol   string   `json:"protocol"`
	ApprovalID string   `json:"approval_id"`
	RunID      string   `json:"run_id"`
	Pipeline   string   `json:"pipeline"`
	Step       string   `json:"step"`
	IssuedAt   string   `json:"issued_at"`
	ExpiresAt  string   `json:"expires_at"`
	Risk       string   `json:"risk,omitempty"`
	Quorum     Quorum   `json:"quorum"`
	Approvers  []string `json:"approvers,omitempty"`
	// PayloadHash is "sha256:<hex>" over the exact bytes of Draft.Body. It is
	// the anchor of the whole security model: a relay that showed the human a
	// different draft than the gate staged cannot echo a matching hash.
	PayloadHash string   `json:"payload_hash"`
	Draft       Draft    `json:"draft"`
	Budget      *Budget  `json:"budget,omitempty"`
	Actions     []Action `json:"actions"`
	Callback    Callback `json:"callback"`
}

type Quorum struct {
	Required  int `json:"required"`
	Collected int `json:"collected"`
}

type Draft struct {
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// Budget is advisory context so the human decides with the day's remaining
// spend in front of them rather than after the fact. Omitted when no cap is
// configured. Unit is whatever the operator's model rates are denominated in —
// draftcat does not assume a currency.
type Budget struct {
	Unit          string  `json:"unit,omitempty"`
	SpentToday    float64 `json:"spent_today"`
	CapToday      float64 `json:"cap_today,omitempty"`
	SpentPipeline float64 `json:"spent_pipeline"`
	CapPipeline   float64 `json:"cap_pipeline,omitempty"`
}

type Action struct {
	Verb    string `json:"verb"`
	Accepts string `json:"accepts,omitempty"`
}

type Callback struct {
	URL   string `json:"url"`
	Nonce string `json:"nonce"`
}

// Decision is the callback envelope: relay -> gate.
//
// There is deliberately no "timeout" decision. Expiry is the gate's call on the
// gate's clock; a relay that could declare a timeout could also starve a gate
// into one.
type Decision struct {
	Protocol    string   `json:"protocol"`
	ApprovalID  string   `json:"approval_id"`
	Nonce       string   `json:"nonce"`
	Decision    string   `json:"decision"`
	PayloadHash string   `json:"payload_hash"`
	Approver    Approver `json:"approver"`
	AdjustText  string   `json:"adjust_text,omitempty"`
	DecidedAt   string   `json:"decided_at"`
}

type Approver struct {
	ID      string `json:"id"`
	Display string `json:"display,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// NewNonce returns a single-use 128-bit callback nonce. Issued at dispatch,
// bound to one approval, burned on first use — so a decision replayed inside
// the clock-skew window still fails.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashPayload returns the "sha256:<hex>" form used in PayloadHash.
func HashPayload(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Sign returns the value for SigHeader over body at time now.
func Sign(secret, body []byte, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a SigHeader value against body. It enforces the
// construction and the clock-skew window only; single-use enforcement lives
// with the nonce in VerifyDecision, because the nonce is bound to a specific
// approval and the signature is not.
//
// Errors are specific for the log. Callers return a bare 401 so a prober learns
// nothing about which check failed.
func VerifySignature(header string, body, secret []byte, maxSkewSeconds int64, now time.Time) error {
	if header == "" {
		return fmt.Errorf("missing %s header", SigHeader)
	}
	var tsPart, sigPart string
	for _, field := range strings.Split(header, ",") {
		field = strings.TrimSpace(field)
		switch {
		case strings.HasPrefix(field, "t="):
			tsPart = strings.TrimPrefix(field, "t=")
		case strings.HasPrefix(field, "v1="):
			sigPart = strings.TrimPrefix(field, "v1=")
		}
	}
	if tsPart == "" || sigPart == "" {
		return fmt.Errorf("malformed signature header (want t=<unix>,v1=<hex>)")
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return fmt.Errorf("unparseable timestamp %q", tsPart)
	}
	if skew := now.Unix() - ts; skew > maxSkewSeconds || skew < -maxSkewSeconds {
		return fmt.Errorf("timestamp outside %ds window (skew %ds)", maxSkewSeconds, skew)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tsPart))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigPart)) != 1 {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// Pending is what the gate remembers about a dispatched approval so it can
// judge the decision that comes back.
type Pending struct {
	ApprovalID  string
	Nonce       string
	PayloadHash string
	// Permitted is the set of approver identities allowed to decide this
	// approval: the channel's allowed operators, narrowed by the step's
	// `approvers:` scope. Membership is checked here, not taken on the relay's
	// word, so a relay cannot invent an approver or widen who may decide.
	Permitted map[string]bool
	ExpiresAt time.Time
}

// VerifyDecision applies every check that stands between an untrusted relay and
// a forged approval, given a signature that already verified. The signature
// (HMAC over the body) and the timestamp window are checked by VerifySignature
// before this; the remaining three are here.
//
// A caller MUST burn p.Nonce after a nil return so the decision is single-use.
func VerifyDecision(d *Decision, p *Pending, now time.Time) error {
	if d.Protocol != Version {
		return fmt.Errorf("protocol %q is not %s", d.Protocol, Version)
	}
	if d.ApprovalID != p.ApprovalID {
		return fmt.Errorf("approval_id %q does not match pending %q", d.ApprovalID, p.ApprovalID)
	}
	// Single-use nonce: replay inside the skew window fails here.
	if subtle.ConstantTimeCompare([]byte(d.Nonce), []byte(p.Nonce)) != 1 {
		return fmt.Errorf("nonce mismatch")
	}
	// Payload-hash echo: this is what makes "the human approved THIS text"
	// verifiable rather than asserted.
	if subtle.ConstantTimeCompare([]byte(d.PayloadHash), []byte(p.PayloadHash)) != 1 {
		return fmt.Errorf("payload_hash mismatch (relay showed a different draft than the gate staged)")
	}
	if now.After(p.ExpiresAt) {
		return fmt.Errorf("gate expired at %s", p.ExpiresAt.UTC().Format(time.RFC3339))
	}
	switch d.Decision {
	case ActionApprove, ActionSkip, ActionAdjust:
	default:
		return fmt.Errorf("decision %q is not one of approve|skip|adjust", d.Decision)
	}
	// Approver membership: the relay reports who decided; the gate decides
	// whether that person was allowed to.
	if d.Approver.ID == "" {
		return fmt.Errorf("decision carries no approver identity")
	}
	if len(p.Permitted) > 0 && !p.Permitted[d.Approver.ID] {
		return fmt.Errorf("approver %q is not permitted to decide this step", d.Approver.ID)
	}
	if d.Decision == ActionAdjust && strings.TrimSpace(d.AdjustText) == "" {
		return fmt.Errorf("adjust decision carries no adjust_text")
	}
	return nil
}

// DecodeDecision parses a decision envelope, refusing oversized bodies before
// they reach the JSON decoder.
func DecodeDecision(body []byte) (*Decision, error) {
	if len(body) > MaxDecisionSize {
		return nil, fmt.Errorf("decision body exceeds %d bytes", MaxDecisionSize)
	}
	var d Decision
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("decode decision: %w", err)
	}
	return &d, nil
}
