package main

// The tool-call gate: `POST /gate/tool-call`, plus `GET /gate/tool-call/<id>`.
//
// draftcat's claim is to be the gate an agent cannot route around. For a
// declared pipeline that is structurally true — the engine runs the steps. For
// a harness that also calls MCP or SDK tools mid-run it was true only by
// convention: those calls never reached the gate at all, so an agent that could
// send an email through a tool could do it without anyone approving.
//
// This endpoint closes that. A harness asks permission for one specific tool
// call and gets a decision back, subject to the same allowlist, policy tiers,
// human approval and audit trail the pipeline steps use.
//
// Properties that matter more than convenience:
//
//   - Default deny. A tool nobody listed is refused, so forgetting to configure
//     a tool fails closed rather than open.
//   - The arguments are hashed into the approval. The human approves THOSE
//     arguments, and a harness that then calls the tool with different ones has
//     an audit trail that does not match.
//   - A rule can constrain the arguments themselves (`args:`). A call outside
//     the constraint never gets MORE freedom than one inside it: on a mismatch
//     the gate either asks a human or refuses, never allows silently.
//   - The gate holds for real harnesses. A decision that needs a human can be
//     collected asynchronously (`mode: async`, or `wait:` for a bounded hold)
//     so an HTTP client timeout does not turn into a lost decision, and the
//     open gate is written to pending_approvals first, so a restart mid-wait
//     is reconciled like any other interrupted gate.
//   - Identical calls do not multiply prompts. Inside repeat_window a call the
//     gate already denied is denied again without asking, an in-flight
//     identical call joins the pending decision, and a cap on repeats stops
//     an agent that loops on the same call from paging the operator forever.
//   - Silent refusals are not silent to the operator. A denial the gate made
//     on its own (unlisted tool, argument mismatch, repeat guard) is reported
//     to the operator channel, deduplicated so a looping agent does not spam.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/approval"
	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// ToolCallRequest is what a harness sends to ask permission.
type ToolCallRequest struct {
	// ActionID is the caller's stable idempotency identity for the intended
	// side effect. Reusing it with the same binding returns the existing state;
	// reusing it with changed inputs is rejected.
	ActionID string `json:"action_id"`
	Tool     string `json:"tool"`
	// Args are the exact arguments the harness intends to call with. They are
	// hashed into the approval record, never stored raw.
	Args map[string]interface{} `json:"args"`
	// RunID optionally ties the call to a pipeline run for the audit trail.
	RunID string `json:"run_id"`
	// Agent names the caller, for the operator's benefit.
	Agent string `json:"agent"`
	// Mode selects how a human decision is delivered. "" or "sync" holds the
	// request until the operator decides or the approval window closes (the
	// v0.5.0 behavior). "async" answers 202 with an approval_id immediately;
	// the harness collects the decision from GET /gate/tool-call/<id>.
	Mode string `json:"mode"`
	// Wait bounds a sync hold ("30s"). When it elapses without a decision the
	// gate answers 202 pending with the approval_id instead of holding on, and
	// the approval keeps running server-side. Ignored in async mode.
	Wait string `json:"wait"`
	// ExpiresAt optionally narrows the permit validity window. The gate never
	// extends it beyond operator_approval.
	ExpiresAt string `json:"expires_at"`

	policyHash       string
	bindingHash      string
	expires          time.Time
	providedActionID bool
	approverID       int64
}

// ToolCallResponse is the gate's answer.
type ToolCallResponse struct {
	ActionID    string `json:"action_id,omitempty"`
	Decision    string `json:"decision"`         // "allow" | "deny" | "pending"
	State       string `json:"state,omitempty"`  // pending | allowed | denied | expired | consumed | revoked
	Permit      string `json:"permit,omitempty"` // "execute" only on the first successful consume
	Reason      string `json:"reason"`
	ArgsHash    string `json:"args_hash"`
	PolicyHash  string `json:"policy_hash,omitempty"`
	BindingHash string `json:"binding_hash,omitempty"`
	DecidedBy   string `json:"decided_by,omitempty"` // "allowlist" | "policy" | "operator" | "repeat-guard"
	// Rule names the rule or condition behind a decision the gate made on its
	// own, so a denial can be traced to config without reading the audit log.
	Rule string `json:"rule,omitempty"`
	// ApprovalID identifies a decision that needs a human. It is set on every
	// human-path response, so a sync caller that later loses the connection
	// can still collect the decision.
	ApprovalID        string `json:"approval_id,omitempty"`
	Poll              string `json:"poll,omitempty"`
	Consume           string `json:"consume,omitempty"`
	ExpiresAt         string `json:"expires_at,omitempty"`
	ExecutionStatus   string `json:"execution_status,omitempty"`
	ResultHash        string `json:"result_hash,omitempty"`
	CompletedAt       string `json:"completed_at,omitempty"`
	ExecutionEvidence string `json:"execution_evidence,omitempty"`
	unavailable       bool
}

const toolGatePath = "/gate/tool-call"

// toolGateStatusWaitCap bounds how long GET /gate/tool-call/<id>?wait= may
// hold. Long enough to be a useful long-poll, short enough that a harness
// behind a 2-minute proxy timeout never sees the proxy give up first.
const toolGateStatusWaitCap = 100 * time.Second

// toolTicketRetention is how long a decided ticket stays collectable.
const toolTicketRetention = time.Hour

// toolTicket is one human-path decision in flight or recently made.
type toolTicket struct {
	ID          string
	Tool        string
	Agent       string
	RunID       string
	ArgsHash    string
	PolicyHash  string
	BindingHash string
	Created     time.Time
	Expires     time.Time

	done       chan struct{}
	mu         sync.Mutex
	decided    bool
	decidedAt  time.Time
	consumedAt time.Time
	resp       ToolCallResponse
	cancel     context.CancelFunc
}

func (t *toolTicket) result() (ToolCallResponse, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resp, t.decided
}

// pendingResponse is what a caller sees while the human has not decided.
func (t *toolTicket) pendingResponse() ToolCallResponse {
	return ToolCallResponse{
		ActionID:    t.ID,
		Decision:    "pending",
		State:       "pending",
		Reason:      "awaiting operator decision",
		ArgsHash:    t.ArgsHash,
		ApprovalID:  t.ID,
		Poll:        toolGatePath + "/" + t.ID,
		Consume:     toolGatePath + "/" + t.ID + "/consume",
		PolicyHash:  t.PolicyHash,
		BindingHash: t.BindingHash,
		ExpiresAt:   t.Expires.UTC().Format(time.RFC3339),
	}
}

// repeatEntry is the gate's memory of one (agent, tool, args) call inside
// repeat_window: the ticket that decided it (or is deciding it) and how many
// times a human has been asked about it.
type repeatEntry struct {
	ticket *toolTicket
	asks   int
	last   time.Time
}

// toolGate is the runtime behind the endpoint: config, the operator channel,
// and the in-memory state the repeat guard, the denial-notice dedup and the
// async tickets need. One per process; tests build their own.
type toolGate struct {
	cfg *config.Config
	ch  OperatorChannel
	now func() time.Time

	mu       sync.Mutex
	tickets  map[string]*toolTicket
	recent   map[string]*repeatEntry
	notified map[string]time.Time
}

func newToolGate(cfg *config.Config, ch OperatorChannel) *toolGate {
	g := &toolGate{
		cfg:      cfg,
		ch:       ch,
		now:      time.Now,
		tickets:  map[string]*toolTicket{},
		recent:   map[string]*repeatEntry{},
		notified: map[string]time.Time{},
	}
	if state != nil {
		if err := state.ExpireToolActions(time.Now()); err != nil {
			log.Printf("[tool-gate] reconcile old permits: %v", err)
		}
	}
	return g
}

// handleToolCall decides one tool call. Kept as the one-line constructor the
// engine and the tests have always used.
//
// The response is intentionally boring: allow or deny plus a reason. A harness
// that cannot parse a rich structure can branch on one string, and a harness
// that ignores the response entirely was never gated to begin with — which is
// why this is a gate the operator puts in front of the tool, not a suggestion
// handed to the model.
func handleToolCall(cfg *config.Config, ch OperatorChannel) http.HandlerFunc {
	return newToolGate(cfg, ch).HandleCall
}

func (g *toolGate) approvalWindow() time.Duration {
	timeout, _ := time.ParseDuration(g.cfg.Timeouts.OperatorApproval)
	if timeout <= 0 {
		timeout = 4 * time.Hour
	}
	return timeout
}

// HandleCall is POST /gate/tool-call.
func (g *toolGate) HandleCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, ok := readRequestBody(w, r, 1<<20)
	if !ok {
		return
	}
	var req ToolCallRequest
	if err := decodeStrictJSON(body, &req); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if req.Tool == "" {
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "deny", Reason: "no tool named",
		})
		return
	}
	req.providedActionID = strings.TrimSpace(req.ActionID) != ""
	if !req.providedActionID {
		// Compatibility for v0.6 clients. Stable retries require callers to
		// persist and resend the returned action_id.
		req.ActionID = newToolTicketID()
	}
	if !validActionID(req.ActionID) {
		http.Error(w, "action_id must be 1-128 URL-safe characters", http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "" && mode != "sync" && mode != "async" {
		http.Error(w, `mode must be "sync" or "async"`, http.StatusBadRequest)
		return
	}
	async := mode == "async"
	var wait time.Duration
	if !async && strings.TrimSpace(req.Wait) != "" {
		d, perr := time.ParseDuration(strings.TrimSpace(req.Wait))
		if perr != nil || d < 0 {
			http.Error(w, `wait must be a duration like "30s"`, http.StatusBadRequest)
			return
		}
		wait = d
	}

	argsHash := hashToolArgs(req.Args)
	rule, listed := g.cfg.ToolGate.Lookup(req.Tool)
	policyHash := g.policyHash(req.Tool, rule, listed)
	expires := g.now().Add(g.approvalWindow())
	if state != nil {
		expires = expires.Truncate(time.Second)
	}
	if strings.TrimSpace(req.ExpiresAt) != "" {
		requested, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil || !requested.After(g.now()) {
			http.Error(w, "expires_at must be a future RFC3339 timestamp", http.StatusBadRequest)
			return
		}
		if requested.Before(expires) {
			expires = requested
		}
	} else {
		g.mu.Lock()
		existing := g.tickets[req.ActionID]
		g.mu.Unlock()
		if existing != nil {
			expires = existing.Expires
		} else if state != nil {
			if a, err := state.ToolAction(req.ActionID); err == nil {
				expires = a.ExpiresAt
			}
		}
	}
	bindingHash := hashToolBinding(req, argsHash, policyHash, expires)
	req.policyHash, req.bindingHash, req.expires = policyHash, bindingHash, expires
	replay, code, handled := g.reserveAction(req, argsHash, policyHash, bindingHash, expires)
	if handled {
		writeToolDecision(w, code, replay)
		return
	}

	// Default deny. An unlisted tool is refused whatever else is true.
	if !listed {
		log.Printf("[tool-gate] DENY %s (not in the allowlist) agent=%q", req.Tool, req.Agent)

		reason := fmt.Sprintf("tool %q is not in tool_gate.tools — the gate denies by default", req.Tool)
		g.notifyDenial(req, argsHash, "unlisted", reason)
		resp := g.finishAction(req, expires, ToolCallResponse{
			Decision: "deny", Reason: reason,
			ArgsHash: argsHash, DecidedBy: "allowlist", Rule: "not listed",
			PolicyHash: policyHash, BindingHash: bindingHash,
		})
		writeToolDecision(w, http.StatusOK, resp)
		return
	}

	// Argument constraints. A mismatch can only tighten: it either escalates
	// to a human or refuses. It never lets a call through that the rule's
	// base setting would have sent to a human.
	needHuman := rule.RequireApproval
	mismatch := ""
	if ok, why := rule.MatchArgs(req.Args); !ok {
		mismatch = why
		if rule.DeniesOnMismatch() {
			reason := "arguments outside the rule — " + why
			log.Printf("[tool-gate] DENY %s (args mismatch: %s) agent=%q", req.Tool, why, req.Agent)

			g.notifyDenial(req, argsHash, "mismatch", reason)
			resp := g.finishAction(req, expires, ToolCallResponse{
				Decision: "deny", Reason: reason,
				ArgsHash: argsHash, DecidedBy: "policy", Rule: "args." + why,
				PolicyHash: policyHash, BindingHash: bindingHash,
			})
			writeToolDecision(w, http.StatusOK, resp)
			return
		}
		needHuman = true
	}

	// A listed tool that does not require approval is allowed on the
	// strength of being listed: that IS an operator decision, made in
	// config and reviewable, and it is recorded as one.
	if !needHuman {
		log.Printf("[tool-gate] ALLOW %s (allowlisted, risk=%s) agent=%q", req.Tool, rule.RiskOf(), req.Agent)

		resp := g.finishAction(req, expires, ToolCallResponse{
			Decision: "allow", Reason: "allowlisted in tool_gate",
			ArgsHash: argsHash, DecidedBy: "allowlist", Rule: "listed",
			PolicyHash: policyHash, BindingHash: bindingHash,
		})
		writeToolDecision(w, http.StatusOK, resp)
		return
	}

	// Needs a human.
	if g.ch == nil {
		resp := g.finishAction(req, expires, ToolCallResponse{
			Decision: "deny", Reason: "tool requires approval but no operator channel is running",
			ArgsHash: argsHash, PolicyHash: policyHash, BindingHash: bindingHash,
		})
		writeToolDecision(w, http.StatusOK, resp)
		return
	}

	// The repeat guard: what does the gate already know about this exact call?
	tk, verdict, ok := g.repeatCheck(req, rule, argsHash)
	if ok {
		// A remembered decision — no prompt.
		verdict.PolicyHash, verdict.BindingHash = policyHash, bindingHash
		writeToolDecision(w, http.StatusOK, g.finishAction(req, expires, verdict))
		return
	}
	if tk != nil && tk.ID != req.ActionID {
		if !req.providedActionID {
			if state != nil {
				if err := state.DecideToolAction(req.ActionID, "denied", "deny", "joined identical pending action", "repeat-guard", g.now()); err != nil {
					writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(req.ActionID))
					return
				}
			}
			writeToolDecision(w, http.StatusAccepted, tk.pendingResponse())
			return
		}
		reason := "an identical action is already pending under action_id " + tk.ID
		resp := g.finishAction(req, expires, ToolCallResponse{
			Decision: "deny", Reason: reason, ArgsHash: argsHash,
			PolicyHash: policyHash, BindingHash: bindingHash,
			DecidedBy: "repeat-guard", Rule: "pending_duplicate",
		})
		writeToolDecision(w, http.StatusConflict, resp)
		return
	}
	if tk == nil {
		// Fresh ask. The approval context is the request's own in the plain
		// sync case (a harness that hangs up cancels the gate, as before) and
		// detached whenever the caller may legitimately come back later.
		tk = g.newTicket(req, argsHash, policyHash, bindingHash, expires)
		parent := r.Context()
		if async || wait > 0 {
			parent = context.Background()
		}
		go g.askHuman(parent, tk, req, rule, mismatch)
	}

	if async {
		writeToolDecision(w, http.StatusAccepted, tk.pendingResponse())
		return
	}
	hold := g.approvalWindow()
	if wait > 0 && wait < hold {
		hold = wait
	}
	select {
	case <-tk.done:
		resp, _ := tk.result()
		writeToolDecision(w, http.StatusOK, resp)
	case <-time.After(hold):
		if wait > 0 {
			writeToolDecision(w, http.StatusAccepted, tk.pendingResponse())
			return
		}
		// The window closed with no decision; the asker resolves the ticket as
		// a timeout deny on its own clock, so report that rather than racing it.
		select {
		case <-tk.done:
		case <-time.After(2 * time.Second):
		}
		resp, decided := tk.result()
		if !decided {
			resp = ToolCallResponse{ActionID: tk.ID, Decision: "deny", State: "denied",
				Reason:   "approval timed out - the gate denies rather than assumes yes",
				ArgsHash: argsHash, PolicyHash: tk.PolicyHash, BindingHash: tk.BindingHash, ApprovalID: tk.ID}
		}
		writeToolDecision(w, http.StatusOK, resp)
	case <-r.Context().Done():
		// The harness hung up. The asker sees the same cancellation in the
		// plain sync case and records the timeout; nothing to write.
	}
}

// HandleStatus serves GET /gate/tool-call/<action_id> and the atomic
// POST /gate/tool-call/<action_id>/consume transition.
func (g *toolGate) HandleStatus(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, toolGatePath), "/")
	parts := strings.Split(rest, "/")
	if rest == "" || len(parts) > 2 || !validActionID(parts[0]) {
		http.Error(w, "approval id required", http.StatusNotFound)
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch parts[1] {
		case "consume":
			g.handleConsume(w, r, id)
		case "revoke":
			g.handleRevoke(w, r, id)
		case "complete":
			g.handleComplete(w, r, id)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	g.mu.Lock()
	tk := g.tickets[id]
	g.mu.Unlock()
	if q := strings.TrimSpace(r.URL.Query().Get("wait")); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil || d < 0 {
			http.Error(w, `wait must be a duration like "30s"`, http.StatusBadRequest)
			return
		}
		if d > toolGateStatusWaitCap {
			d = toolGateStatusWaitCap
		}
		if tk != nil {
			select {
			case <-tk.done:
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
	}
	resp, code := g.actionResponse(id)
	writeToolDecision(w, code, resp)
}

func (g *toolGate) handleConsume(w http.ResponseWriter, r *http.Request, id string) {
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
	resp, code := g.actionResponse(id)
	if code != http.StatusOK {
		writeToolDecision(w, code, resp)
		return
	}
	if body.BindingHash == "" || body.BindingHash != resp.BindingHash || resp.State != "allowed" {
		resp.Permit = ""
		writeToolDecision(w, http.StatusConflict, resp)
		return
	}
	a, err := state.ToolAction(id)
	if err != nil {
		writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		return
	}
	now := g.now()
	e, err := toolConsumptionReceipt(a, now)
	if err != nil {
		writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		return
	}
	a, consumed, err := state.ConsumeToolActionWithReceipt(id, body.BindingHash, now, e)
	if err != nil {
		writeToolDecision(w, http.StatusServiceUnavailable, unavailableToolResponse(id))
		return
	}
	resp = responseFromToolAction(a)
	g.updateTicket(id, resp)
	if !consumed {
		writeToolDecision(w, http.StatusConflict, resp)
		return
	}
	resp.Decision, resp.Permit = "allow", "execute"
	resp.ExecutionStatus = "unreported"
	resp.Reason = "permit consumed; execute this bound action once"
	writeToolDecision(w, http.StatusOK, resp)
}

// newTicket registers a fresh human-path decision and remembers the call for
// the repeat guard. Also the moment old tickets are swept.
func (g *toolGate) newTicket(req ToolCallRequest, argsHash, policyHash, bindingHash string, expires time.Time) *toolTicket {
	now := g.now()
	tk := &toolTicket{
		ID: req.ActionID, Tool: req.Tool, Agent: req.Agent, RunID: req.RunID, ArgsHash: argsHash,
		PolicyHash: policyHash, BindingHash: bindingHash,
		Created: now, Expires: expires, done: make(chan struct{}),
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Sweep: decided tickets past retention, repeat-guard memory past its
	// window, denial-notice dedup past its window. All three are bounded by
	// the rate of distinct calls, so a busy gate does not grow without limit.
	for id, old := range g.tickets {
		old.mu.Lock()
		stale := (old.decided && now.Sub(old.decidedAt) > toolTicketRetention) ||
			(!old.decided && now.Sub(old.Expires) > toolTicketRetention)
		old.mu.Unlock()
		if stale {
			delete(g.tickets, id)
		}
	}
	if window := g.cfg.ToolGate.RepeatWindowOrDefault(); window > 0 {
		for k, e := range g.recent {
			if now.Sub(e.last) > window {
				delete(g.recent, k)
			}
		}
	}
	if window := g.cfg.ToolGate.NotifyWindowOrDefault(); window > 0 {
		for k, at := range g.notified {
			if now.Sub(at) > window {
				delete(g.notified, k)
			}
		}
	}
	g.tickets[tk.ID] = tk
	key := repeatKey(req, argsHash)
	e := g.recent[key]
	if e == nil {
		e = &repeatEntry{}
		g.recent[key] = e
	}
	e.ticket = tk
	e.asks++
	e.last = now
	return tk
}

func repeatKey(req ToolCallRequest, argsHash string) string {
	return req.Agent + "\x00" + req.Tool + "\x00" + argsHash
}

// repeatCheck consults the gate's memory of this exact call. It returns:
//
//   - (ticket, _, false) when an identical call is still being decided — the
//     caller joins it instead of opening a second prompt;
//   - (nil, verdict, true) when the memory settles it without a human: a
//     denial inside the window, a remembered approval, or the repeat cap;
//   - (nil, _, false) when the human must be asked afresh.
func (g *toolGate) repeatCheck(req ToolCallRequest, rule config.ToolRule, argsHash string) (*toolTicket, ToolCallResponse, bool) {
	window := g.cfg.ToolGate.RepeatWindowOrDefault()
	if window <= 0 {
		return nil, ToolCallResponse{}, false
	}
	now := g.now()
	key := repeatKey(req, argsHash)

	g.mu.Lock()
	e := g.recent[key]
	if e != nil && now.Sub(e.last) > window {
		delete(g.recent, key)
		e = nil
	}
	var tk *toolTicket
	asks := 0
	if e != nil {
		tk = e.ticket
		asks = e.asks
	}
	g.mu.Unlock()
	if tk == nil {
		return nil, ToolCallResponse{}, false
	}

	resp, decided := tk.result()
	if !decided {
		return tk, ToolCallResponse{}, false
	}
	ago := now.Sub(tk.decidedAt).Round(time.Second)
	switch resp.Decision {
	case "deny":
		reason := fmt.Sprintf("repeat of a call denied %s ago (%s) — not asking again inside repeat_window", ago, resp.Reason)
		log.Printf("[tool-gate] DENY %s (repeat guard: denied %s ago) agent=%q", req.Tool, ago, req.Agent)

		g.notifyDenial(req, argsHash, "repeat", reason)
		return nil, ToolCallResponse{Decision: "deny", Reason: reason, ArgsHash: argsHash,
			DecidedBy: "repeat-guard", Rule: "repeat_window", ApprovalID: tk.ID}, true
	case "allow":
		if rule.RememberApproval {
			reason := fmt.Sprintf("identical call approved %s ago — remember_approval reuses it inside repeat_window", ago)
			log.Printf("[tool-gate] ALLOW %s (repeat guard: approved %s ago) agent=%q", req.Tool, ago, req.Agent)

			return nil, ToolCallResponse{Decision: "allow", Reason: reason, ArgsHash: argsHash,
				DecidedBy: "repeat-guard", Rule: "remember_approval", ApprovalID: tk.ID}, true
		}
	}
	if capN := g.cfg.ToolGate.MaxRepeats; capN > 0 && asks >= capN {
		reason := fmt.Sprintf("asked %d times inside repeat_window — max_repeats reached, not asking again", asks)
		log.Printf("[tool-gate] DENY %s (repeat guard: %d asks) agent=%q", req.Tool, asks, req.Agent)

		g.notifyDenial(req, argsHash, "max_repeats", reason)
		return nil, ToolCallResponse{Decision: "deny", Reason: reason, ArgsHash: argsHash,
			DecidedBy: "repeat-guard", Rule: "max_repeats"}, true
	}
	return nil, ToolCallResponse{}, false
}

// askHuman runs one human-path decision to its end and resolves the ticket.
// The gate is written to pending_approvals before the prompt leaves, so a
// process that dies mid-wait is reconciled at next boot like a pipeline gate.
func (g *toolGate) askHuman(parent context.Context, tk *toolTicket, req ToolCallRequest, rule config.ToolRule, mismatch string) {
	pendingID, err := state.BeginApproval("tool-gate", req.Tool, tk.ArgsHash, 1, tk.Created, tk.Expires)
	if err != nil {
		g.updateTicket(tk.ID, unavailableToolResponse(tk.ID))
		return
	}
	draft := fmt.Sprintf("[draftcat] Tool call awaiting approval\n\nagent: %s\ntool:  %s\nrisk:  %s\nargs:  %s", orDash(req.Agent), req.Tool, rule.RiskOf(), prettyArgs(req.Args))
	if mismatch != "" {
		draft += "\nrule:  arguments outside the rule — " + mismatch
	}
	draft += "\n\nargs sha256: " + tk.ArgsHash
	timeout := tk.Expires.Sub(g.now())
	ctx, cancel := withGateMetaTimeout(parent, req.RunID, req.Tool, rule.RiskOf(), timeout)
	defer cancel()
	tk.mu.Lock()
	if tk.decided {
		tk.mu.Unlock()
		_ = state.ResolveApproval(pendingID)
		return
	}
	tk.cancel = cancel
	tk.mu.Unlock()
	dec, derr := g.ch.SendForApproval(ctx, draft, nil)
	if err := state.ResolveApproval(pendingID); err != nil {
		g.updateTicket(tk.ID, unavailableToolResponse(tk.ID))
		return
	}
	if resp, decided := tk.result(); decided && resp.State != "pending" {
		return
	}
	req.approverID = dec.ApproverID
	resp := ToolCallResponse{Decision: "deny", Reason: "operator did not approve", ArgsHash: tk.ArgsHash, PolicyHash: tk.PolicyHash, BindingHash: tk.BindingHash, DecidedBy: "operator"}
	switch {
	case derr != nil:
		resp.Reason = "approval failed: " + derr.Error()
	case dec.Action == "timeout":
		resp.Reason = "approval timed out — the gate denies rather than assumes yes"
	case dec.Action == "approve":
		resp.Decision, resp.Reason = "allow", "operator approved"
	}
	g.finishAction(req, tk.Expires, resp)
}

// notifyDenial tells the operator about a refusal the gate made on its own.
// One notice per agent+tool+class inside notify_window: an agent that loops
// on a denied call produces one line, not a page of them.
func (g *toolGate) notifyDenial(req ToolCallRequest, argsHash, class, reason string) {
	if g.ch == nil || !g.cfg.ToolGate.NotifyDenialsOn() {
		return
	}
	now := g.now()
	key := req.Agent + "\x00" + req.Tool + "\x00" + class
	g.mu.Lock()
	if last, seen := g.notified[key]; seen && now.Sub(last) < g.cfg.ToolGate.NotifyWindowOrDefault() {
		g.mu.Unlock()
		return
	}
	g.notified[key] = now
	g.mu.Unlock()

	msg := fmt.Sprintf("[tool-gate] DENIED %s (agent: %s)\nreason: %s\nargs sha256: %s",
		req.Tool, orDash(req.Agent), reason, argsHash)
	if err := g.ch.Send(msg); err != nil {
		log.Printf("[tool-gate] denial notice failed: %v", err)
	}
}

func newToolTicketID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is a broken host; fall back to time so the gate
		// keeps working, and the id is still unique per process.
		return fmt.Sprintf("tc_%d", time.Now().UnixNano())
	}
	return "tc_" + hex.EncodeToString(b)
}

// withGateMetaTimeout builds the approval context for a tool call, carrying the
// same run/step/risk metadata a pipeline gate would, so a relay renders a tool
// call and a pipeline step the same way.
func withGateMetaTimeout(parent context.Context, runID, tool, risk string, timeout time.Duration) (context.Context, func()) {
	base := withRun(parent, runID, "tool-gate")
	base = withStep(base, tool)
	base = withGateMeta(base, risk, budgetSnapshot{})
	return context.WithTimeout(base, timeout)
}

// hashToolArgs canonicalises the arguments and hashes them, so an approval is
// bound to the exact call the harness proposed. encoding/json sorts map keys,
// which is what makes the hash stable across callers.
func hashToolArgs(args map[string]interface{}) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validActionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func (g *toolGate) policyHash(tool string, rule config.ToolRule, listed bool) string {
	b, _ := json.Marshal(struct {
		ToolPolicy     string
		Enabled        bool
		ApprovalWindow int64
		Telegram       config.TelegramConfig
		Relay          config.RelayConfig
	}{hashToolPolicy(tool, rule, listed), g.cfg.ToolGate.Enabled, int64(g.approvalWindow()), g.cfg.Telegram, g.cfg.Relay})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashToolPolicy(tool string, rule config.ToolRule, listed bool) string {
	b, _ := json.Marshal(struct {
		Tool   string          `json:"tool"`
		Listed bool            `json:"listed"`
		Rule   config.ToolRule `json:"rule"`
	}{Tool: tool, Listed: listed, Rule: rule})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashToolBinding(req ToolCallRequest, argsHash, policyHash string, expires time.Time) string {
	b, _ := json.Marshal(struct {
		Version    int    `json:"version"`
		ActionID   string `json:"action_id"`
		Agent      string `json:"agent"`
		Tool       string `json:"tool"`
		RunID      string `json:"run_id"`
		ArgsHash   string `json:"args_hash"`
		PolicyHash string `json:"policy_hash"`
		ExpiresAt  int64  `json:"expires_at"`
	}{2, req.ActionID, req.Agent, req.Tool, req.RunID, argsHash, policyHash, expires.Unix()})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func responseFromToolAction(a statestore.ToolAction) ToolCallResponse {
	decision := a.Decision
	if decision == "" {
		decision = "pending"
	}
	resp := ToolCallResponse{
		ActionID: a.ActionID, Decision: decision, State: a.Status, Reason: a.Reason,
		ArgsHash: a.ArgsHash, PolicyHash: a.PolicyHash, BindingHash: a.BindingHash,
		DecidedBy: a.DecidedBy, ApprovalID: a.ActionID,
		Poll:      toolGatePath + "/" + a.ActionID,
		ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if a.Status == "pending" && resp.Reason == "" {
		resp.Reason = "awaiting operator decision"
	}
	if a.Status == "allowed" {
		resp.Consume = toolGatePath + "/" + a.ActionID + "/consume"
		if resp.Reason == "" {
			resp.Reason = "decision allows this action; consume the permit before executing"
		}
	}

	resp.ExecutionStatus = "not_started"
	if a.Status == "consumed" {
		resp.ExecutionStatus = "unreported"
		resp.Permit = ""
		resp.Reason = "permit already consumed"
	}
	if a.Status == "expired" || a.Status == "revoked" {
		resp.Decision = "deny"
	}
	return resp
}

// reserveAction is the idempotency gate. The first request reserves its action
// id; matching retries return the existing state and drift is rejected.
func (g *toolGate) reserveAction(req ToolCallRequest, argsHash, policyHash, bindingHash string, expires time.Time) (ToolCallResponse, int, bool) {
	if state != nil {
		a, created, err := state.ReserveToolAction(statestore.ToolAction{ActionID: req.ActionID, Tool: req.Tool, Agent: req.Agent, RunID: req.RunID, ArgsHash: argsHash, PolicyHash: policyHash, BindingHash: bindingHash, CreatedAt: g.now(), UpdatedAt: g.now(), ExpiresAt: expires})
		if err != nil {
			if a.ActionID != "" && a.BindingHash != bindingHash {
				return ToolCallResponse{ActionID: req.ActionID, Decision: "deny", State: "denied", Reason: "action_id is already bound to different action data"}, http.StatusConflict, true
			}
			return unavailableToolResponse(req.ActionID), http.StatusServiceUnavailable, true
		}
		if !created {
			resp, code := g.actionResponse(req.ActionID)
			if resp.State == "pending" && code == http.StatusOK {
				code = http.StatusAccepted
			}
			return resp, code, true
		}
		return ToolCallResponse{}, 0, false
	}
	g.mu.Lock()
	tk := g.tickets[req.ActionID]
	g.mu.Unlock()
	if tk == nil {
		return ToolCallResponse{}, 0, false
	}
	if tk.BindingHash != bindingHash {
		return ToolCallResponse{ActionID: req.ActionID, Decision: "deny", State: "denied", Reason: "action_id is already bound to different action data"}, http.StatusConflict, true
	}
	resp, code := g.actionResponse(req.ActionID)
	if resp.State == "pending" && code == http.StatusOK {
		code = http.StatusAccepted
	}
	return resp, code, true
}

func (g *toolGate) finishAction(req ToolCallRequest, expires time.Time, resp ToolCallResponse) ToolCallResponse {
	now := g.now()
	resp.ActionID, resp.ApprovalID = req.ActionID, req.ActionID
	resp.ExpiresAt = expires.UTC().Format(time.RFC3339)
	resp.Poll = toolGatePath + "/" + req.ActionID
	resp.State = "denied"
	decision := "deny"
	if resp.Decision == "allow" {
		resp.State = "allowed"
		resp.Consume = toolGatePath + "/" + req.ActionID + "/consume"
		resp.Reason += "; consume the permit before executing"
		decision = "policy_approve"
		if resp.DecidedBy == "operator" {
			decision = "approve"
		}
	}
	if !now.Before(expires) {
		resp.Decision, resp.State, resp.Consume = "deny", "expired", ""
		resp.Reason = "permit expired before consumption"
		decision = "deny"
	}
	if state != nil {
		e, err := toolDecisionReceipt(req, resp.ArgsHash, decision, req.approverID, resp.Reason, now)
		if err != nil {
			resp = unavailableToolResponse(req.ActionID)
		} else {
			a, err := state.DecideToolActionWithReceipt(req.ActionID, map[string]string{"allow": "allowed", "deny": "denied"}[resp.Decision], resp.Decision, resp.Reason, resp.DecidedBy, now, e)
			if err != nil {
				resp = unavailableToolResponse(req.ActionID)
			} else {
				old := resp
				resp = responseFromToolAction(a)
				if a.Status == old.State {
					resp.Rule = old.Rule
				}
			}
		}
	}
	obs.RecordApproval("tool-gate", req.Tool, decision)
	g.mu.Lock()
	tk := g.tickets[req.ActionID]
	if tk == nil {
		tk = &toolTicket{ID: req.ActionID, Tool: req.Tool, Agent: req.Agent, RunID: req.RunID, ArgsHash: hashToolArgs(req.Args), PolicyHash: req.policyHash, BindingHash: req.bindingHash, Created: now, Expires: expires, done: make(chan struct{})}
		g.tickets[tk.ID] = tk
	}
	g.mu.Unlock()
	g.updateTicket(req.ActionID, resp)
	return resp
}

func toolDecisionReceipt(req ToolCallRequest, argsHash, decision string, operator int64, reason string, decidedAt time.Time) (statestore.ApprovalEnvelope, error) {
	receiptID := "rcpt_" + strings.TrimPrefix(newToolTicketID(), "tc_")
	expires := req.expires
	if expires.IsZero() {
		expires = decidedAt.Add(4 * time.Hour)
	}
	quorumGot := 0
	if decision == "approve" || decision == "policy_approve" {
		quorumGot = 1
	}
	envelope := statestore.ApprovalEnvelope{
		ReceiptID: receiptID, RunID: req.RunID, ActionID: req.ActionID,
		Pipeline: "tool-gate", Step: req.Tool, DecidedAt: decidedAt,
		Decision: decision, OperatorID: operator, PayloadHash: argsHash,
		QuorumN: 1, QuorumGot: quorumGot, Policy: reason, PolicyHash: req.policyHash,
		BindingHash: req.bindingHash, ExpiresAt: expires, Lifecycle: "decided",
	}
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	if len(secret) > 0 {
		nonce, err := approval.NewNonce()
		if err == nil {
			envelope.Nonce = nonce
			envelope.Signature = approval.SignV2(secret, approval.FieldsV2{
				ReceiptID: envelope.ReceiptID, RunID: envelope.RunID, ActionID: envelope.ActionID,
				Pipeline: envelope.Pipeline, Step: envelope.Step, DecidedAt: envelope.DecidedAt.Unix(),
				Decision: envelope.Decision, OperatorID: envelope.OperatorID, PayloadHash: envelope.PayloadHash,
				Policy: envelope.Policy, PolicyHash: envelope.PolicyHash, BindingHash: envelope.BindingHash,
				ExpiresAt: envelope.ExpiresAt.Unix(), QuorumN: envelope.QuorumN, QuorumGot: envelope.QuorumGot,
			}, nonce)
		}
	}
	if len(secret) > 0 && (envelope.Nonce == "" || envelope.Signature == "") {
		return envelope, errors.New("approval receipt signing failed")
	}
	return envelope, nil
}

func toolConsumptionReceipt(a statestore.ToolAction, at time.Time) (statestore.ApprovalEnvelope, error) {
	e := statestore.ApprovalEnvelope{
		ReceiptID: "rcpt_" + strings.TrimPrefix(newToolTicketID(), "tc_"),
		RunID:     a.RunID, ActionID: a.ActionID, Pipeline: "tool-gate", Step: a.Tool,
		DecidedAt: at, Decision: "consume", PayloadHash: a.ArgsHash,
		QuorumN: 1, QuorumGot: 1, Policy: "permit-consume", PolicyHash: a.PolicyHash,
		BindingHash: a.BindingHash, ExpiresAt: a.ExpiresAt, Lifecycle: "consumed",
	}
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	if len(secret) > 0 {
		if nonce, err := approval.NewNonce(); err == nil {
			e.Nonce = nonce
			e.Signature = approval.SignV2(secret, approval.FieldsV2{
				ReceiptID: e.ReceiptID, RunID: e.RunID, ActionID: e.ActionID,
				Pipeline: e.Pipeline, Step: e.Step, DecidedAt: e.DecidedAt.Unix(),
				Decision: e.Decision, OperatorID: e.OperatorID, PayloadHash: e.PayloadHash,
				Policy: e.Policy, PolicyHash: e.PolicyHash, BindingHash: e.BindingHash,
				ExpiresAt: e.ExpiresAt.Unix(), QuorumN: e.QuorumN, QuorumGot: e.QuorumGot,
			}, nonce)
		}
	}
	if len(secret) > 0 && (e.Nonce == "" || e.Signature == "") {
		return e, errors.New("approval receipt signing failed")
	}
	return e, nil
}

func writeToolDecision(w http.ResponseWriter, code int, resp ToolCallResponse) {
	if resp.unavailable {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

func prettyArgs(args map[string]interface{}) string {
	if len(args) == 0 {
		return "(none)"
	}
	b, err := json.MarshalIndent(args, "       ", "  ")
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
