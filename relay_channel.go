package main

// RelayChannel implements OperatorChannel over the hitl/v0 protocol
// (internal/relay, spec in docs/hitl-protocol.md).
//
// It is the answer to "add a Teams channel" that does not put a vendor's bot
// lifecycle inside draftcat. The gate dispatches a signed approval request to
// whatever presenter the operator already trusts, that presenter shows it to a
// human on its own surface, and it posts one signed decision back. draftcat
// keeps everything that must not be delegated: who may decide, how many must
// decide, when the gate expires, what exact bytes were shown, and the audit
// record of what happened.
//
// The relay never holds draftcat's connection open. Dispatch is a short POST
// the relay acknowledges immediately; the human's decision arrives later on the
// callback server. That is what makes a four-hour approval window work over a
// transport with a thirty-second timeout.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/channels"
	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/relay"
)

// RelayChannel is an OperatorChannel that speaks hitl/v0.
type RelayChannel struct {
	cfg    config.RelayConfig
	secret []byte
	client *http.Client

	mu      sync.Mutex
	pending map[string]*relayGate // approval_id -> live gate
	srv     *http.Server
}

// relayGate is one dispatched approval awaiting decisions. Quorum is collected
// here: distinct approver identities accumulate until the required count is
// met, and any skip or adjust resolves the gate immediately — the same
// semantics the Telegram quorum gate already uses.
type relayGate struct {
	pending   relay.Pending
	need      int
	approvers []int64
	seen      map[string]bool
	done      chan QuorumDecision
	once      sync.Once
}

// resolve delivers the final decision exactly once. Later callbacks for the
// same gate get a 409 from the handler rather than racing on a closed channel.
func (g *relayGate) resolve(d QuorumDecision) {
	g.once.Do(func() { g.done <- d })
}

// NewRelayChannel builds the channel and starts its callback server. The server
// is part of the channel, not an optional extra: a relay with nowhere to post a
// decision is a channel that cannot complete a round trip, which is exactly
// what internal/channels exists to keep out of the config surface.
func NewRelayChannel(cfg config.RelayConfig) (*RelayChannel, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("relay channel: url is empty")
	}
	if cfg.Secret() == "" {
		return nil, fmt.Errorf("relay channel: secret (%s) is empty — dispatches cannot be signed", cfg.SecretEnv)
	}
	if strings.TrimSpace(cfg.PublicURL) == "" {
		return nil, fmt.Errorf("relay channel: public_url is required — the relay cannot post a decision to a loopback address")
	}
	addr := cfg.CallbackAddr
	if addr == "" {
		addr = "127.0.0.1:8089"
	}

	rc := &RelayChannel{
		cfg:     cfg,
		secret:  []byte(cfg.Secret()),
		client:  &http.Client{Timeout: 30 * time.Second},
		pending: map[string]*relayGate{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(relay.CallbackPath, rc.handleDecision)
	rc.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := rc.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[relay] callback server stopped: %v", err)
		}
	}()
	log.Printf("[relay] callback server listening on %s%s (public: %s%s)", addr, relay.CallbackPath, cfg.PublicURL, relay.CallbackPath)
	return rc, nil
}

// Close stops the callback server.
func (r *RelayChannel) Close() error {
	if r.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.srv.Shutdown(ctx)
}

func (r *RelayChannel) Name() string { return channels.Relay }

// Send posts a plain notification: a dispatch with no actions and no callback
// nonce, which a relay renders without asking for a decision.
func (r *RelayChannel) Send(text string) error {
	req := relay.Request{
		Protocol:    relay.Version,
		Pipeline:    "-",
		Step:        "notify",
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
		PayloadHash: relay.HashPayload(text),
		Draft:       relay.Draft{ContentType: "text/plain", Body: text},
		Actions:     []relay.Action{},
	}
	_, err := r.dispatch(context.Background(), req)
	return err
}

// SendForApproval is the single-approver gate: quorum of one.
func (r *RelayChannel) SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error) {
	qd, err := r.SendForQuorumApproval(ctx, draft, 1, approvers)
	if err != nil {
		return OperatorDecision{}, err
	}
	var id int64
	if qd.Action == relay.ActionApprove && len(qd.Approvers) > 0 {
		id = qd.Approvers[0]
	}
	return OperatorDecision{Action: qd.Action, Text: qd.Text, ApproverID: id}, nil
}

// SendForQuorumApproval dispatches the draft and blocks until `need` distinct
// permitted approvers approve, any of them skips or adjusts, or ctx expires.
//
// ctx carries the operator-approval timeout the engine already computed, so
// expiry stays on the gate's clock. A relay that goes silent produces a
// timeout, and a timeout does not fire the action.
func (r *RelayChannel) SendForQuorumApproval(ctx context.Context, draft string, need int, approvers []int64) (QuorumDecision, error) {
	if need < 1 {
		need = 1
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(4 * time.Hour)
	}

	nonce, err := relay.NewNonce()
	if err != nil {
		return QuorumDecision{Action: "timeout"}, err
	}
	approvalID, err := relay.NewNonce()
	if err != nil {
		return QuorumDecision{Action: "timeout"}, err
	}

	// Permitted = the channel's allowed operators, narrowed by the step's
	// approver scope. Built here, checked on callback: the relay is told who to
	// present to, but never gets to decide who counts.
	permittedIDs := approvers
	if len(permittedIDs) == 0 {
		permittedIDs = r.cfg.Security.AllowedUsers
	}
	permitted := map[string]bool{}
	wire := make([]string, 0, len(permittedIDs))
	for _, id := range permittedIDs {
		if ident := r.cfg.IdentityFor(id); ident != "" {
			permitted[ident] = true
			wire = append(wire, ident)
		}
	}
	if len(permitted) == 0 {
		return QuorumDecision{Action: "timeout"}, fmt.Errorf("relay: no operator identities configured for the permitted approvers — gate would be unsatisfiable")
	}
	if need > len(permitted) {
		return QuorumDecision{Action: "timeout"}, fmt.Errorf("relay: quorum %d exceeds %d permitted approver(s) — unsatisfiable", need, len(permitted))
	}

	hash := relay.HashPayload(draft)
	gate := &relayGate{
		pending: relay.Pending{
			ApprovalID:  approvalID,
			Nonce:       nonce,
			PayloadHash: hash,
			Permitted:   permitted,
			ExpiresAt:   deadline,
		},
		need: need,
		seen: map[string]bool{},
		done: make(chan QuorumDecision, 1),
	}
	r.mu.Lock()
	r.pending[approvalID] = gate
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, approvalID)
		r.mu.Unlock()
	}()

	req := relay.Request{
		Protocol:    relay.Version,
		ApprovalID:  approvalID,
		RunID:       runIDFromContext(ctx),
		Pipeline:    pipelineFromContext(ctx),
		Step:        stepFromContext(ctx),
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:   deadline.UTC().Format(time.RFC3339),
		Quorum:      relay.Quorum{Required: need},
		Approvers:   wire,
		PayloadHash: hash,
		Draft:       relay.Draft{ContentType: "text/plain", Body: draft},
		Actions: []relay.Action{
			{Verb: relay.ActionApprove},
			{Verb: relay.ActionSkip},
			{Verb: relay.ActionAdjust, Accepts: "text"},
		},
		Callback: relay.Callback{
			URL:   strings.TrimRight(r.cfg.PublicURL, "/") + relay.CallbackPath,
			Nonce: nonce,
		},
	}

	if _, err := r.dispatch(ctx, req); err != nil {
		return QuorumDecision{Action: "timeout"}, fmt.Errorf("relay dispatch failed: %w", err)
	}

	select {
	case d := <-gate.done:
		return d, nil
	case <-ctx.Done():
		// Expiry is the gate's call. The engine records this exactly as it
		// records a Telegram timeout, and the action does not fire.
		return QuorumDecision{Action: "timeout"}, nil
	}
}

// dispatch signs and POSTs one envelope. It deliberately does not wait for the
// human: a relay must ack promptly and carry the wait on its own side.
func (r *RelayChannel) dispatch(ctx context.Context, req relay.Request) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal dispatch: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(relay.ProtocolHeader, relay.Version)
	httpReq.Header.Set(relay.SigHeader, relay.Sign(r.secret, body, time.Now()))

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("relay returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// handleDecision receives one decision envelope from the relay.
//
// Failures return a bare status with no detail: a prober must not learn which
// check rejected it. The specific reason goes to the log.
func (r *RelayChannel) handleDecision(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, relay.MaxDecisionSize+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// 1+2. Body-bound HMAC and clock-skew window, before anything is parsed.
	if err := relay.VerifySignature(req.Header.Get(relay.SigHeader), body, r.secret, relay.MaxSkewSeconds, time.Now()); err != nil {
		log.Printf("[relay] decision rejected: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	d, err := relay.DecodeDecision(body)
	if err != nil {
		log.Printf("[relay] decision rejected: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	gate, live := r.pending[d.ApprovalID]
	r.mu.Unlock()
	if !live {
		// Either already resolved or never existed. Both are a 409 so a retry
		// is safe and idempotent, and neither leaks which it was.
		writeRelayJSON(w, http.StatusConflict, map[string]any{"status": "already_resolved"})
		return
	}

	// 3+4+5. Nonce, payload-hash echo, approver membership, expiry, verb.
	if err := relay.VerifyDecision(d, &gate.pending, time.Now()); err != nil {
		log.Printf("[relay] decision rejected for %s: %v", d.ApprovalID, err)
		if strings.Contains(err.Error(), "expired") {
			writeRelayJSON(w, http.StatusGone, map[string]any{"status": "expired"})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}

	opID, known := r.cfg.OperatorFor(d.Approver.ID)
	if !known {
		log.Printf("[relay] decision rejected for %s: approver %q is not a configured operator", d.ApprovalID, d.Approver.ID)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch d.Decision {
	case relay.ActionSkip:
		gate.resolve(QuorumDecision{Action: "skip"})
		writeRelayJSON(w, http.StatusOK, map[string]any{"status": "recorded", "resolved": true})
		return
	case relay.ActionAdjust:
		gate.resolve(QuorumDecision{Action: "adjust", Text: d.AdjustText})
		writeRelayJSON(w, http.StatusOK, map[string]any{"status": "recorded", "resolved": true})
		return
	}

	// Approve. Quorum counts DISTINCT approvers: one person tapping twice does
	// not satisfy a two-of-three gate.
	if gate.seen[d.Approver.ID] {
		writeRelayJSON(w, http.StatusOK, map[string]any{"status": "recorded", "resolved": false, "note": "duplicate approver"})
		return
	}
	gate.seen[d.Approver.ID] = true
	gate.approvers = append(gate.approvers, opID)

	if len(gate.seen) >= gate.need {
		gate.resolve(QuorumDecision{Action: "approve", Approvers: append([]int64(nil), gate.approvers...)})
		writeRelayJSON(w, http.StatusOK, map[string]any{"status": "recorded", "resolved": true})
		return
	}
	writeRelayJSON(w, http.StatusOK, map[string]any{
		"status": "recorded", "resolved": false,
		"collected": len(gate.seen), "required": gate.need,
	})
}

func writeRelayJSON(w http.ResponseWriter, code int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
