package main

// `draftcat hitl verify <relay-url>` — the hitl/v0 conformance suite.
//
// A protocol nobody can test is a document. This dispatches a real approval to
// a live relay and checks the decision that comes back against every rule the
// gate enforces at runtime, so a relay author can find out whether their Power
// Automate flow (or bot, or shell script) is correct BEFORE it stands between
// an agent and a customer.
//
// It also proves the negative half of the contract, which is the half that
// matters: a conformant relay must not be able to get a replayed decision, a
// mutated payload hash, or an out-of-scope approver past the gate. Those cases
// are checked locally against the same verification code the callback handler
// runs, so the suite reports on the gate's actual behavior rather than a
// re-implementation of it.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/relay"
)

type hitlCheck struct {
	name string
	ok   bool
	note string
}

func runHitlCmd(args []string) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: draftcat hitl verify <relay-url> [--timeout 120s] [--identity you@example.com] [--json]")
		return 2
	}
	fs := flag.NewFlagSet("hitl verify", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 120*time.Second, "how long to wait for the relay to return a decision")
	identity := fs.String("identity", "conformance@draftcat.test", "approver identity the relay is expected to report")
	addr := fs.String("addr", "127.0.0.1:8099", "local address to listen for the callback on")
	publicURL := fs.String("public-url", "", "externally reachable base URL for the callback (default http://<addr>)")
	jsonOut := fs.Bool("json", false, "emit machine-readable results")
	// Go's flag package stops parsing at the first non-flag argument, so
	// `hitl verify <url> --identity x` would silently ignore every flag after
	// the URL and run with defaults instead. Silently running a conformance
	// suite against settings the operator did not ask for is the same class of
	// failure as an approval routed to a channel nobody is watching, so pull
	// the positional out first and let flags appear on either side of it.
	var positional []string
	var flagArgs []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			// A flag written as `--key value` consumes the next token; `--key=value`
			// and the bool --json do not.
			if !strings.Contains(a, "=") && !strings.HasSuffix(a, "json") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "error: relay URL is required")
		return 2
	}
	relayURL := positional[0]

	secret := os.Getenv("DRAFTCAT_RELAY_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "error: DRAFTCAT_RELAY_SECRET must be set — the suite signs its dispatch and verifies the decision with it")
		return 2
	}
	base := *publicURL
	if base == "" {
		base = "http://" + *addr
	}

	var checks []hitlCheck
	add := func(name string, ok bool, note string) { checks = append(checks, hitlCheck{name, ok, note}) }

	nonce, _ := relay.NewNonce()
	approvalID, _ := relay.NewNonce()
	draft := "draftcat hitl/v0 conformance probe — approving this executes nothing."
	hash := relay.HashPayload(draft)
	deadline := time.Now().Add(*timeout)

	pending := relay.Pending{
		ApprovalID:  approvalID,
		Nonce:       nonce,
		PayloadHash: hash,
		Permitted:   map[string]bool{*identity: true},
		ExpiresAt:   deadline,
	}

	// Callback listener. Records the first decision that arrives, verbatim.
	var (
		mu       sync.Mutex
		got      *relay.Decision
		gotRaw   []byte
		gotSig   string
		received = make(chan struct{}, 1)
	)
	mux := http.NewServeMux()
	mux.HandleFunc(relay.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, relay.MaxDecisionSize+1))
		mu.Lock()
		if got == nil {
			gotRaw = body
			gotSig = r.Header.Get(relay.SigHeader)
			if d, err := relay.DecodeDecision(body); err == nil {
				got = d
			}
			select {
			case received <- struct{}{}:
			default:
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"recorded","resolved":true}`))
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
	defer func() { _ = srv.Close() }()

	req := relay.Request{
		Protocol: relay.Version, ApprovalID: approvalID,
		RunID: "conformance", Pipeline: "conformance", Step: "verify",
		IssuedAt: time.Now().UTC().Format(time.RFC3339), ExpiresAt: deadline.UTC().Format(time.RFC3339),
		Quorum: relay.Quorum{Required: 1}, Approvers: []string{*identity},
		PayloadHash: hash,
		Draft:       relay.Draft{ContentType: "text/plain", Body: draft},
		Actions: []relay.Action{
			{Verb: relay.ActionApprove}, {Verb: relay.ActionSkip}, {Verb: relay.ActionAdjust, Accepts: "text"},
		},
		Callback: relay.Callback{URL: strings.TrimRight(base, "/") + relay.CallbackPath, Nonce: nonce},
	}
	body, _ := json.Marshal(req)

	// Check 1: the relay acks promptly and does NOT hold the connection for the
	// human. A relay that blocks here cannot serve a four-hour approval window.
	dispatchStart := time.Now()
	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dispatchCancel()
	httpReq, _ := http.NewRequestWithContext(dispatchCtx, http.MethodPost, relayURL, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(relay.ProtocolHeader, relay.Version)
	httpReq.Header.Set(relay.SigHeader, relay.Sign([]byte(secret), body, time.Now()))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(httpReq)
	ackDur := time.Since(dispatchStart)
	if err != nil {
		add("dispatch accepted", false, err.Error())
		return reportHitl(checks, *jsonOut)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	add("dispatch accepted", resp.StatusCode >= 200 && resp.StatusCode <= 299,
		fmt.Sprintf("HTTP %d in %s", resp.StatusCode, ackDur.Round(time.Millisecond)))
	add("ack does not block on the human", ackDur < 25*time.Second,
		fmt.Sprintf("acked in %s", ackDur.Round(time.Millisecond)))

	fmt.Fprintf(os.Stderr, "waiting up to %s for a decision on %s%s ...\n", *timeout, base, relay.CallbackPath)
	select {
	case <-received:
	case <-time.After(time.Until(deadline)):
		add("decision received", false, "no decision arrived before the deadline")
		return reportHitl(checks, *jsonOut)
	}
	add("decision received", got != nil, "")
	if got == nil {
		add("decision is well-formed JSON", false, "body did not decode as a hitl/v0 decision")
		return reportHitl(checks, *jsonOut)
	}

	// Check 2: the decision is signed, body-bound, and inside the clock window.
	sigErr := relay.VerifySignature(gotSig, gotRaw, []byte(secret), relay.MaxSkewSeconds, time.Now())
	add("decision is signed and in-window", sigErr == nil, errNote(sigErr))

	// Check 3: it passes every gate rule.
	verErr := relay.VerifyDecision(got, &pending, time.Now())
	add("decision passes gate verification", verErr == nil, errNote(verErr))
	if verErr == nil {
		add("decision verb is in the v0 set", true, got.Decision)
		add("approver identity reported", got.Approver.ID != "", got.Approver.ID)
	}

	// Check 4: the payload-hash echo. This is the check that makes "the human
	// approved THIS text" verifiable, so it is called out on its own.
	add("payload hash echoed unchanged", got.PayloadHash == hash, got.PayloadHash)

	// Check 5: the negatives. A conformant relay must not be able to get these
	// past the gate — verified against the same code the callback handler runs.
	replay := *got
	add("replayed decision is refused",
		relay.VerifyDecision(&replay, &relay.Pending{
			ApprovalID: approvalID, Nonce: "burned", PayloadHash: hash,
			Permitted: pending.Permitted, ExpiresAt: deadline,
		}, time.Now()) != nil, "nonce burned after first use")

	mutated := *got
	mutated.PayloadHash = relay.HashPayload("a different draft than the human saw")
	add("mutated payload hash is refused",
		relay.VerifyDecision(&mutated, &pending, time.Now()) != nil, "")

	outsider := *got
	outsider.Approver.ID = "outsider@example.com"
	add("out-of-scope approver is refused",
		relay.VerifyDecision(&outsider, &pending, time.Now()) != nil, "")

	expired := pending
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	add("decision after expiry is refused",
		relay.VerifyDecision(got, &expired, time.Now()) != nil, "")

	return reportHitl(checks, *jsonOut)
}

func errNote(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func reportHitl(checks []hitlCheck, jsonOut bool) int {
	failed := 0
	for _, c := range checks {
		if !c.ok {
			failed++
		}
	}
	if jsonOut {
		type row struct {
			Check string `json:"check"`
			OK    bool   `json:"ok"`
			Note  string `json:"note,omitempty"`
		}
		out := struct {
			Protocol   string `json:"protocol"`
			Conformant bool   `json:"conformant"`
			Failed     int    `json:"failed"`
			Checks     []row  `json:"checks"`
		}{Protocol: relay.Version, Conformant: failed == 0, Failed: failed}
		for _, c := range checks {
			out.Checks = append(out.Checks, row{c.Check(), c.ok, c.note})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Printf("\nhitl/v0 conformance\n")
		for _, c := range checks {
			mark := "PASS"
			if !c.ok {
				mark = "FAIL"
			}
			line := fmt.Sprintf("  [%s] %s", mark, c.name)
			if c.note != "" {
				line += "  — " + c.note
			}
			fmt.Println(line)
		}
		if failed == 0 {
			fmt.Printf("\nconformant: %d/%d checks passed\n", len(checks), len(checks))
		} else {
			fmt.Printf("\nNOT conformant: %d of %d checks failed\n", failed, len(checks))
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// Check returns the check's display name (method form keeps the JSON encoder
// from needing an exported field on an internal type).
func (c hitlCheck) Check() string { return c.name }
