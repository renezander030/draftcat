package main

// The tool-call gate: `POST /gate/tool-call`.
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
// Two properties matter more than convenience:
//
//   - Default deny. A tool nobody listed is refused, so forgetting to configure
//     a tool fails closed rather than open.
//   - The arguments are hashed into the approval. The human approves THOSE
//     arguments, and a harness that then calls the tool with different ones has
//     an audit trail that does not match.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/renezander030/draftcat/internal/config"
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
}

// ToolCallResponse is the gate's answer.
type ToolCallResponse struct {
	Decision  string `json:"decision"` // "allow" | "deny"
	Reason    string `json:"reason"`
	ArgsHash  string `json:"args_hash"`
	DecidedBy string `json:"decided_by,omitempty"` // "allowlist" | "policy" | "operator"
}

const toolGatePath = "/gate/tool-call"

// handleToolCall decides one tool call.
//
// The response is intentionally boring: allow or deny plus a reason. A harness
// that cannot parse a rich structure can branch on one string, and a harness
// that ignores the response entirely was never gated to begin with — which is
// why this is a gate the operator puts in front of the tool, not a suggestion
// handed to the model.
func handleToolCall(cfg *config.Config, ch OperatorChannel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			writeToolDecision(w, http.StatusBadRequest, ToolCallResponse{
				Decision: "deny", Reason: "no tool named",
			})
			return
		}

		argsHash := hashToolArgs(req.Args)
		rule, listed := cfg.ToolGate.Lookup(req.Tool)

		// Default deny. An unlisted tool is refused whatever else is true.
		if !listed {
			log.Printf("[tool-gate] DENY %s (not in the allowlist) agent=%q", req.Tool, req.Agent)
			recordToolDecision(cfg, req, argsHash, "deny", 0, "unlisted")
			writeToolDecision(w, http.StatusOK, ToolCallResponse{
				Decision: "deny",
				Reason:   fmt.Sprintf("tool %q is not in tool_gate.tools — the gate denies by default", req.Tool),
				ArgsHash: argsHash, DecidedBy: "allowlist",
			})
			return
		}

		// A listed tool that does not require approval is allowed on the
		// strength of being listed: that IS an operator decision, made in
		// config and reviewable, and it is recorded as one.
		if !rule.RequireApproval {
			log.Printf("[tool-gate] ALLOW %s (allowlisted, risk=%s) agent=%q", req.Tool, rule.RiskOf(), req.Agent)
			recordToolDecision(cfg, req, argsHash, "policy_approve", 0, "allowlisted risk="+rule.RiskOf())
			writeToolDecision(w, http.StatusOK, ToolCallResponse{
				Decision: "allow", Reason: "allowlisted in tool_gate",
				ArgsHash: argsHash, DecidedBy: "allowlist",
			})
			return
		}

		// Needs a human. Hold the request for the approval window; a harness
		// asking permission is expected to wait for the answer.
		if ch == nil {
			writeToolDecision(w, http.StatusOK, ToolCallResponse{
				Decision: "deny", Reason: "tool requires approval but no operator channel is running",
				ArgsHash: argsHash,
			})
			return
		}
		timeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
		if timeout == 0 {
			timeout = 4 * time.Hour
		}
		draft := fmt.Sprintf("[draftcat] Tool call awaiting approval\n\nagent: %s\ntool:  %s\nrisk:  %s\nargs:  %s\n\nargs sha256: %s",
			orDash(req.Agent), req.Tool, rule.RiskOf(), prettyArgs(req.Args), argsHash)

		ctx, cancel := withGateMetaTimeout(r.Context(), req.RunID, req.Tool, rule.RiskOf(), timeout)
		defer cancel()
		dec, derr := ch.SendForApproval(ctx, draft, nil)
		if derr != nil || dec.Action != "approve" {
			reason := "operator did not approve"
			if derr != nil {
				reason = "approval failed: " + derr.Error()
			} else if dec.Action == "timeout" {
				reason = "approval timed out — the gate denies rather than assumes yes"
			}
			log.Printf("[tool-gate] DENY %s (%s) agent=%q", req.Tool, reason, req.Agent)
			recordToolDecision(cfg, req, argsHash, "deny", dec.ApproverID, reason)
			writeToolDecision(w, http.StatusOK, ToolCallResponse{
				Decision: "deny", Reason: reason, ArgsHash: argsHash, DecidedBy: "operator",
			})
			return
		}

		log.Printf("[tool-gate] ALLOW %s (operator %d) agent=%q", req.Tool, dec.ApproverID, req.Agent)
		recordToolDecision(cfg, req, argsHash, "approve", dec.ApproverID, "operator approved")
		writeToolDecision(w, http.StatusOK, ToolCallResponse{
			Decision: "allow", Reason: "operator approved", ArgsHash: argsHash, DecidedBy: "operator",
		})
	}
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
// bound to the exact call the harness proposed. Raw arguments are never stored:
// they routinely carry customer data, and the audit trail's job is to prove
// what was approved, not to become a second copy of it.
func hashToolArgs(args map[string]interface{}) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func recordToolDecision(cfg *config.Config, req ToolCallRequest, argsHash, decision string, operator int64, reason string) {
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
