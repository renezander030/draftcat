package main

// The hitl/v0 side of the relay: the envelopes and the signature scheme, as
// docs/hitl-protocol.md specifies them. This module is stdlib-only and does not
// import draftcat, so the few functions it needs are restated here; the
// conformance suite (`draftcat hitl verify`) is what holds the two in step.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	protocolVer    = "hitl/v0"
	sigHeader      = "X-Draftcat-Signature"
	protoHeader    = "X-Hitl-Protocol"
	maxSkewSeconds = 300

	verbApprove = "approve"
	verbSkip    = "skip"
	verbAdjust  = "adjust"
)

// dispatch is the subset of the hitl/v0 request this relay reads. Unknown
// fields are ignored by design, so a newer draftcat can add context without
// breaking a relay already deployed.
type dispatch struct {
	Protocol    string   `json:"protocol"`
	ApprovalID  string   `json:"approval_id"`
	RunID       string   `json:"run_id"`
	Pipeline    string   `json:"pipeline"`
	Step        string   `json:"step"`
	IssuedAt    string   `json:"issued_at"`
	ExpiresAt   string   `json:"expires_at"`
	Risk        string   `json:"risk"`
	Quorum      quorum   `json:"quorum"`
	Approvers   []string `json:"approvers"`
	PayloadHash string   `json:"payload_hash"`
	Draft       struct {
		ContentType string `json:"content_type"`
		Body        string `json:"body"`
	} `json:"draft"`
	Budget   *budget  `json:"budget"`
	Actions  []action `json:"actions"`
	Callback struct {
		URL   string `json:"url"`
		Nonce string `json:"nonce"`
	} `json:"callback"`
}

type quorum struct {
	Required  int `json:"required"`
	Collected int `json:"collected"`
}

type budget struct {
	Unit          string  `json:"unit"`
	SpentToday    float64 `json:"spent_today"`
	CapToday      float64 `json:"cap_today"`
	SpentPipeline float64 `json:"spent_pipeline"`
	CapPipeline   float64 `json:"cap_pipeline"`
}

type action struct {
	Verb    string `json:"verb"`
	Accepts string `json:"accepts,omitempty"`
}

// decision is the relay -> gate envelope.
type decision struct {
	Protocol    string `json:"protocol"`
	ApprovalID  string `json:"approval_id"`
	Nonce       string `json:"nonce"`
	Decision    string `json:"decision"`
	PayloadHash string `json:"payload_hash"`
	Approver    struct {
		ID      string `json:"id"`
		Display string `json:"display,omitempty"`
		Channel string `json:"channel,omitempty"`
	} `json:"approver"`
	AdjustText string `json:"adjust_text,omitempty"`
	DecidedAt  string `json:"decided_at"`
}

// isApproval reports whether the dispatch asks for a decision. A dispatch with
// no callback nonce is a plain notification: render it, ask nothing.
func (d *dispatch) isApproval() bool { return d.Callback.Nonce != "" }

// verbs returns the decisions this relay can offer for d, in a fixed order.
// Verbs outside the v0 set are ignored rather than rendered, so a newer gate
// cannot get a button this relay does not know how to answer.
func (d *dispatch) verbs() []string {
	offered := map[string]bool{}
	for _, a := range d.Actions {
		offered[a.Verb] = true
	}
	var out []string
	for _, v := range []string{verbApprove, verbSkip, verbAdjust} {
		if offered[v] {
			out = append(out, v)
		}
	}
	return out
}

func (d *dispatch) offers(verb string) bool {
	for _, v := range d.verbs() {
		if v == verb {
			return true
		}
	}
	return false
}

// hashPayload returns the "sha256:<hex>" form draftcat puts in payload_hash.
func hashPayload(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sign returns the X-Draftcat-Signature value over body at time now.
func sign(secret, body []byte, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature checks an X-Draftcat-Signature value: the HMAC over
// "<t>.<body>" and the clock window around t.
func verifySignature(secret []byte, header string, body []byte, now time.Time) error {
	if header == "" {
		return fmt.Errorf("missing %s", sigHeader)
	}
	var tsPart, sigPart string
	for _, f := range strings.Split(header, ",") {
		f = strings.TrimSpace(f)
		switch {
		case strings.HasPrefix(f, "t="):
			tsPart = strings.TrimPrefix(f, "t=")
		case strings.HasPrefix(f, "v1="):
			sigPart = strings.TrimPrefix(f, "v1=")
		}
	}
	if tsPart == "" || sigPart == "" {
		return errors.New("malformed signature header")
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return errors.New("unparseable signature timestamp")
	}
	if skew := now.Unix() - ts; skew > maxSkewSeconds || skew < -maxSkewSeconds {
		return fmt.Errorf("signature timestamp outside %ds window", maxSkewSeconds)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tsPart))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigPart)) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}
