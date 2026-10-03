package main

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

func unavailableToolResponse(id string) ToolCallResponse {
	return ToolCallResponse{ActionID: id, ApprovalID: id, Decision: "deny", State: "denied", Reason: "durable approval state or audit unavailable; no execution authorized", unavailable: true}
}

// updateTicket wakes all waiters on a terminal state and interrupts the
// operator prompt. Persisted state decides the race, not prompt completion.
func (g *toolGate) updateTicket(id string, resp ToolCallResponse) {
	g.mu.Lock()
	tk := g.tickets[id]
	g.mu.Unlock()
	if tk == nil {
		return
	}
	tk.mu.Lock()
	if (tk.resp.State == "revoked" || tk.resp.State == "consumed" || tk.resp.State == "expired") && (resp.State == "allowed" || resp.State == "pending") {
		tk.mu.Unlock()
		return
	}
	if resp.ArgsHash == "" {
		resp.ArgsHash = tk.ArgsHash
	}
	if resp.BindingHash == "" {
		resp.BindingHash = tk.BindingHash
	}
	if resp.PolicyHash == "" {
		resp.PolicyHash = tk.PolicyHash
	}
	tk.resp = resp
	terminal := resp.State != "pending"
	if terminal && !tk.decided {
		tk.decided = true
		tk.decidedAt = g.now()
		close(tk.done)
	}
	cancel := tk.cancel
	tk.mu.Unlock()
	if terminal && cancel != nil {
		cancel()
	}
}

func (g *toolGate) actionResponse(id string) (ToolCallResponse, int) {
	if state != nil {
		a, err := state.ToolAction(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return unknownToolResponse(id), http.StatusNotFound
			}
			return unavailableToolResponse(id), http.StatusServiceUnavailable
		}
		rule, listed := g.cfg.ToolGate.Lookup(a.Tool)
		a, err = state.NormalizeToolAction(id, g.policyHash(a.Tool, rule, listed), g.now())
		if err != nil {
			return unavailableToolResponse(id), http.StatusServiceUnavailable
		}
		if a.Status == "pending" {
			g.mu.Lock()
			tk := g.tickets[id]
			g.mu.Unlock()
			if tk != nil {
				if failed, _ := tk.result(); failed.unavailable {
					return failed, http.StatusServiceUnavailable
				}
			}
		}
		resp := responseFromToolAction(a)
		resp.ExecutionStatus = "not_started"
		if a.Status == "consumed" {
			resp.ExecutionStatus = "unreported"
		}
		o, err := state.ToolOutcome(id)
		if err == nil {
			resp.ExecutionStatus = o.Status
			resp.ResultHash = o.ResultHash
			resp.CompletedAt = o.CompletedAt.UTC().Format(time.RFC3339)
			resp.ExecutionEvidence = "caller_attestation"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return unavailableToolResponse(id), http.StatusServiceUnavailable
		}
		g.updateTicket(id, resp)
		return resp, http.StatusOK
	}
	g.mu.Lock()
	tk := g.tickets[id]
	g.mu.Unlock()
	if tk == nil {
		return unknownToolResponse(id), http.StatusNotFound
	}
	resp, decided := tk.result()
	if !decided {
		resp = tk.pendingResponse()
	}
	if resp.State == "pending" || resp.State == "allowed" {
		rule, listed := g.cfg.ToolGate.Lookup(tk.Tool)
		reason := ""
		if tk.PolicyHash != g.policyHash(tk.Tool, rule, listed) {
			reason = "policy changed; request a new approval"
		} else if !g.now().Before(tk.Expires) {
			reason = "permit expired before consumption"
		}
		if reason != "" {
			resp.Decision, resp.State, resp.Consume, resp.Permit, resp.Reason = "deny", "expired", "", "", reason
			g.updateTicket(id, resp)
		}
	}
	return resp, http.StatusOK
}

func unknownToolResponse(id string) ToolCallResponse {
	return ToolCallResponse{ActionID: id, ApprovalID: id, Decision: "deny", State: "denied", Reason: "unknown action id - the gate has no decision for it; ask again"}
}

func (g *toolGate) handleRevoke(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		BindingHash string `json:"binding_hash"`
	}
	raw, ok := readRequestBody(w, r, 4096)
	if !ok {
		return
	}
	if err := decodeStrictJSON(raw, &body); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if state == nil {
		writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		return
	}
	if body.BindingHash == "" {
		http.Error(w, "binding_hash required", http.StatusBadRequest)
		return
	}
	a, revoked, err := state.CancelToolAction(id, body.BindingHash, g.now())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeToolDecision(w, http.StatusNotFound, unknownToolResponse(id))
		} else {
			writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		}
		return
	}
	resp := responseFromToolAction(a)
	g.updateTicket(id, resp)
	if !revoked {
		writeToolDecision(w, http.StatusConflict, resp)
		return
	}
	writeToolDecision(w, http.StatusOK, resp)
}

func validResultHash(hash string) bool {
	if hash == "" {
		return true
	}
	if !strings.HasPrefix(hash, "sha256:") || len(hash) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	return err == nil
}

func (g *toolGate) handleComplete(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		BindingHash string `json:"binding_hash"`
		Status      string `json:"status"`
		ResultHash  string `json:"result_hash"`
	}
	raw, ok := readRequestBody(w, r, 4096)
	if !ok {
		return
	}
	if err := decodeStrictJSON(raw, &body); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if body.BindingHash == "" || (body.Status != "succeeded" && body.Status != "failed") || !validResultHash(body.ResultHash) {
		http.Error(w, "binding_hash and status succeeded|failed required; result_hash must be SHA-256", http.StatusBadRequest)
		return
	}
	if state == nil {
		writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		return
	}
	_, accepted, err := state.CompleteToolAction(statestore.ToolOutcome{ActionID: id, BindingHash: body.BindingHash, Status: body.Status, ResultHash: body.ResultHash, CompletedAt: g.now()})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeToolDecision(w, http.StatusNotFound, unknownToolResponse(id))
		} else {
			writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		}
		return
	}
	resp, code := g.actionResponse(id)
	if !accepted && code == http.StatusOK {
		code = http.StatusConflict
	}
	writeToolDecision(w, code, resp)
}
