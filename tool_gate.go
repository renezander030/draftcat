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
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
)

// ToolCallRequest is what a harness sends to ask permission.
type ToolCallRequest struct {
	Tool string `json:"tool"`
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
}

// ToolCallResponse is the gate's answer.
type ToolCallResponse struct {
	Decision  string `json:"decision"` // "allow" | "deny" | "pending"
	Reason    string `json:"reason"`
	ArgsHash  string `json:"args_hash"`
	DecidedBy string `json:"decided_by,omitempty"` // "allowlist" | "policy" | "operator" | "repeat-guard"
	// Rule names the rule or condition behind a decision the gate made on its
	// own, so a denial can be traced to config without reading the audit log.
	Rule string `json:"rule,omitempty"`
	// ApprovalID identifies a decision that needs a human. It is set on every
	// human-path response, so a sync caller that later loses the connection
	// can still collect the decision.
	ApprovalID string `json:"approval_id,omitempty"`
	Poll       string `json:"poll,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
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
	ID       string
	Tool     string
	Agent    string
	RunID    string
	ArgsHash string
	Created  time.Time
	Expires  time.Time

	done      chan struct{}
	mu        sync.Mutex
	decided   bool
	decidedAt time.Time
	resp      ToolCallResponse
}

func (t *toolTicket) result() (ToolCallResponse, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resp, t.decided
}

func (t *toolTicket) resolve(resp ToolCallResponse, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.decided {
		return
	}
	t.decided = true
	t.decidedAt = at
	t.resp = resp
	close(t.done)
}

// pendingResponse is what a caller sees while the human has not decided.
func (t *toolTicket) pendingResponse() ToolCallResponse {
	return ToolCallResponse{
		Decision:   "pending",
		Reason:     "awaiting operator decision",
		ArgsHash:   t.ArgsHash,
		ApprovalID: t.ID,
		Poll:       toolGatePath + "/" + t.ID,
		ExpiresAt:  t.Expires.UTC().Format(time.RFC3339),
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
	return &toolGate{
		cfg:      cfg,
		ch:       ch,
		now:      time.Now,
		tickets:  map[string]*toolTicket{},
		recent:   map[string]*repeatEntry{},
		notified: map[string]time.Time{},
	}
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req ToolCallRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if req.Tool == "" {
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "deny", Reason: "no tool named",
		})
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

	// Default deny. An unlisted tool is refused whatever else is true.
	if !listed {
		log.Printf("[tool-gate] DENY %s (not in the allowlist) agent=%q", req.Tool, req.Agent)
		recordToolDecision(g.cfg, req, argsHash, "deny", 0, "unlisted")
		reason := fmt.Sprintf("tool %q is not in tool_gate.tools — the gate denies by default", req.Tool)
		g.notifyDenial(req, argsHash, "unlisted", reason)
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "deny", Reason: reason,
			ArgsHash: argsHash, DecidedBy: "allowlist", Rule: "not listed",
		})
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
			recordToolDecision(g.cfg, req, argsHash, "deny", 0, "args mismatch: "+why)
			g.notifyDenial(req, argsHash, "mismatch", reason)
			writeToolDecision(w, http.StatusOK, ToolCallResponse{
				Decision: "deny", Reason: reason,
				ArgsHash: argsHash, DecidedBy: "policy", Rule: "args." + why,
			})
			return
		}
		needHuman = true
	}

	// A listed tool that does not require approval is allowed on the
	// strength of being listed: that IS an operator decision, made in
	// config and reviewable, and it is recorded as one.
	if !needHuman {
		log.Printf("[tool-gate] ALLOW %s (allowlisted, risk=%s) agent=%q", req.Tool, rule.RiskOf(), req.Agent)
		recordToolDecision(g.cfg, req, argsHash, "policy_approve", 0, "allowlisted risk="+rule.RiskOf())
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "allow", Reason: "allowlisted in tool_gate",
			ArgsHash: argsHash, DecidedBy: "allowlist", Rule: "listed",
		})
		return
	}

	// Needs a human.
	if g.ch == nil {
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "deny", Reason: "tool requires approval but no operator channel is running",
			ArgsHash: argsHash,
		})
		return
	}

	// The repeat guard: what does the gate already know about this exact call?
	tk, verdict, ok := g.repeatCheck(req, rule, argsHash)
	if ok {
		// A remembered decision — no prompt.
		writeToolDecision(w, http.StatusOK, verdict)
		return
	}
	if tk == nil {
		// Fresh ask. The approval context is the request's own in the plain
		// sync case (a harness that hangs up cancels the gate, as before) and
		// detached whenever the caller may legitimately come back later.
		tk = g.newTicket(req, argsHash)
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
			resp = ToolCallResponse{Decision: "deny", Reason: "approval timed out — the gate denies rather than assumes yes",
				ArgsHash: argsHash, ApprovalID: tk.ID}
		}
		writeToolDecision(w, http.StatusOK, resp)
	case <-r.Context().Done():
		// The harness hung up. The asker sees the same cancellation in the
		// plain sync case and records the timeout; nothing to write.
	}
}

// HandleStatus is GET /gate/tool-call/<approval_id>[?wait=30s].
func (g *toolGate) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, toolGatePath), "/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "approval id required", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	tk := g.tickets[id]
	g.mu.Unlock()
	if tk == nil {
		// Unknown here means the gate restarted or the id is stale. Either
		// way there is no decision to hand over, and the safe answer is no.
		writeToolDecision(w, http.StatusNotFound, ToolCallResponse{
			Decision: "deny", Reason: "unknown or expired approval id — the gate has no decision for it; ask again",
			ApprovalID: id,
		})
		return
	}
	if q := strings.TrimSpace(r.URL.Query().Get("wait")); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil || d < 0 {
			http.Error(w, `wait must be a duration like "30s"`, http.StatusBadRequest)
			return
		}
		if d > toolGateStatusWaitCap {
			d = toolGateStatusWaitCap
		}
		select {
		case <-tk.done:
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	if resp, decided := tk.result(); decided {
		writeToolDecision(w, http.StatusOK, resp)
		return
	}
	writeToolDecision(w, http.StatusOK, tk.pendingResponse())
}

// newTicket registers a fresh human-path decision and remembers the call for
// the repeat guard. Also the moment old tickets are swept.
func (g *toolGate) newTicket(req ToolCallRequest, argsHash string) *toolTicket {
	now := g.now()
	tk := &toolTicket{
		ID: newToolTicketID(), Tool: req.Tool, Agent: req.Agent, RunID: req.RunID, ArgsHash: argsHash,
		Created: now, Expires: now.Add(g.approvalWindow()), done: make(chan struct{}),
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
		recordToolDecision(g.cfg, req, argsHash, "deny", 0, "repeat-guard: denied "+ago.String()+" ago")
		g.notifyDenial(req, argsHash, "repeat", reason)
		return nil, ToolCallResponse{Decision: "deny", Reason: reason, ArgsHash: argsHash,
			DecidedBy: "repeat-guard", Rule: "repeat_window", ApprovalID: tk.ID}, true
	case "allow":
		if rule.RememberApproval {
			reason := fmt.Sprintf("identical call approved %s ago — remember_approval reuses it inside repeat_window", ago)
			log.Printf("[tool-gate] ALLOW %s (repeat guard: approved %s ago) agent=%q", req.Tool, ago, req.Agent)
			recordToolDecision(g.cfg, req, argsHash, "policy_approve", 0, "repeat-guard: remembered approval from "+ago.String()+" ago")
			return nil, ToolCallResponse{Decision: "allow", Reason: reason, ArgsHash: argsHash,
				DecidedBy: "repeat-guard", Rule: "remember_approval", ApprovalID: tk.ID}, true
		}
	}
	if capN := g.cfg.ToolGate.MaxRepeats; capN > 0 && asks >= capN {
		reason := fmt.Sprintf("asked %d times inside repeat_window — max_repeats reached, not asking again", asks)
		log.Printf("[tool-gate] DENY %s (repeat guard: %d asks) agent=%q", req.Tool, asks, req.Agent)
		recordToolDecision(g.cfg, req, argsHash, "deny", 0, fmt.Sprintf("repeat-guard: max_repeats %d reached", capN))
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
	timeout := g.approvalWindow()
	pendingID, perr := state.BeginApproval("tool-gate", req.Tool, tk.ArgsHash, 1, tk.Created, tk.Expires)
	if perr != nil {
		log.Printf("[tool-gate] pending-approval write failed: %v", perr)
	}

	draft := fmt.Sprintf("[draftcat] Tool call awaiting approval\n\nagent: %s\ntool:  %s\nrisk:  %s\nargs:  %s",
		orDash(req.Agent), req.Tool, rule.RiskOf(), prettyArgs(req.Args))
	if mismatch != "" {
		draft += "\nrule:  arguments outside the rule — " + mismatch
	}
	draft += "\n\nargs sha256: " + tk.ArgsHash

	ctx, cancel := withGateMetaTimeout(parent, req.RunID, req.Tool, rule.RiskOf(), timeout)
	defer cancel()
	dec, derr := g.ch.SendForApproval(ctx, draft, nil)

	if rerr := state.ResolveApproval(pendingID); rerr != nil {
		log.Printf("[tool-gate] pending-approval resolve failed: %v", rerr)
	}

	now := g.now()
	if derr != nil || dec.Action != "approve" {
		reason := "operator did not approve"
		if derr != nil {
			reason = "approval failed: " + derr.Error()
		} else if dec.Action == "timeout" {
			reason = "approval timed out — the gate denies rather than assumes yes"
		}
		log.Printf("[tool-gate] DENY %s (%s) agent=%q", req.Tool, reason, req.Agent)
		recordToolDecision(g.cfg, req, tk.ArgsHash, "deny", dec.ApproverID, reason)
		tk.resolve(ToolCallResponse{
			Decision: "deny", Reason: reason, ArgsHash: tk.ArgsHash, DecidedBy: "operator", ApprovalID: tk.ID,
		}, now)
		return
	}

	log.Printf("[tool-gate] ALLOW %s (operator %d) agent=%q", req.Tool, dec.ApproverID, req.Agent)
	recordToolDecision(g.cfg, req, tk.ArgsHash, "approve", dec.ApproverID, "operator approved")
	tk.resolve(ToolCallResponse{
		Decision: "allow", Reason: "operator approved", ArgsHash: tk.ArgsHash, DecidedBy: "operator", ApprovalID: tk.ID,
	}, now)
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

func recordToolDecision(cfg *config.Config, req ToolCallRequest, argsHash, decision string, operator int64, reason string) {
	obs.RecordApproval("tool-gate", req.Tool, decision)
	if state == nil {
		return
	}
	if err := state.RecordApprovalRow(req.RunID, "tool-gate", req.Tool, time.Now(),
		decision, operator, argsHash, 1, 1, "", "", reason); err != nil {
		log.Printf("[tool-gate] audit write failed: %v", err)
	}
	_ = cfg
}

func writeToolDecision(w http.ResponseWriter, code int, resp ToolCallResponse) {
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
