package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The X-Draftcat-Signature receipt, both sides: the hook signs with it, the
// webhook and tool gate endpoints verify it. One module owns the format.

// webhookSigHeader carries the body-bound HMAC receipt on inbound triggers.
const webhookSigHeader = "X-Draftcat-Signature"

// bearerMatches reports whether an Authorization header carries exactly the
// webhook token, compared in constant time.
func bearerMatches(header string, secret []byte) bool {
	const prefix = "Bearer "
	return strings.HasPrefix(header, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), secret) == 1
}

// verifyWebhookSignature checks the Stripe/GitHub-style receipt
//
//	X-Draftcat-Signature: t=<unix>,v1=<hex hmac-sha256(t + "." + body)>
//
// A bearer token proves only that the caller once saw the token: it does not
// bind the request body, and a captured header replays forever. Since a webhook
// here STARTS a pipeline, that is the one thing an inbound request must not be
// able to do on its own. Signing t+body and bounding the clock skew closes both
// holes; the nonce store below closes replay inside the skew window.
//
// Errors are deliberately specific for the log and generic for the caller — the
// handler returns a bare 401 so a prober learns nothing about which check failed.
func verifyWebhookSignature(header string, body, secret []byte, maxSkewSeconds int64, now time.Time) error {
	sig, err := authenticateWebhookSignature(header, body, secret, maxSkewSeconds, now)
	if err != nil {
		return err
	}
	return claimWebhookSignature(sig, now)
}

func claimWebhookSignature(sig string, now time.Time) error {
	if state != nil {
		won, err := state.TryMarkSeen(webhookReplayScope, "sig", sig, now)
		if err != nil {
			return fmt.Errorf("replay record failed: %w", err)
		}
		if !won {
			return fmt.Errorf("signature already used (replay)")
		}
	}
	return nil
}

func authenticateWebhookSignature(header string, body, secret []byte, maxSkewSeconds int64, now time.Time) (string, error) {
	if header == "" {
		return "", fmt.Errorf("missing %s header", webhookSigHeader)
	}
	var tsPart, sigPart string
	for _, field := range strings.Split(header, ",") {
		field = strings.TrimSpace(field)
		switch {
		case strings.HasPrefix(field, "t="):
			if tsPart != "" {
				return "", fmt.Errorf("duplicate timestamp")
			}
			tsPart = strings.TrimPrefix(field, "t=")
		case strings.HasPrefix(field, "v1="):
			if sigPart != "" {
				return "", fmt.Errorf("duplicate signature")
			}
			sigPart = strings.TrimPrefix(field, "v1=")
		}
	}
	if tsPart == "" || sigPart == "" {
		return "", fmt.Errorf("malformed signature header")
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return "", fmt.Errorf("unparseable timestamp")
	}
	// Compare bounds instead of subtracting an attacker-controlled timestamp.
	if ts < now.Unix()-maxSkewSeconds || ts > now.Unix()+maxSkewSeconds {
		return "", fmt.Errorf("timestamp outside %ds window", maxSkewSeconds)
	}
	want := webhookMAC(secret, tsPart, body)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigPart)) != 1 {
		return "", fmt.Errorf("signature mismatch")
	}
	return sigPart, nil
}

// webhookReplayScope namespaces spent webhook signatures in seen_items. The
// leading underscores keep it from colliding with a real pipeline name.
const webhookReplayScope = "__webhook"

// signHookBody is the X-Draftcat-Signature value the gate verifies:
// t=<unix>,v1=<hex hmac-sha256(t + "." + body)>, keyed with the bearer token.
func signHookBody(key []byte, ts int64, body []byte) string {
	t := strconv.FormatInt(ts, 10)
	return "t=" + t + ",v1=" + webhookMAC(key, t, body)
}

// webhookMAC is the v1 value: hex hmac-sha256 over t + "." + body.
func webhookMAC(key []byte, t string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(t))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
