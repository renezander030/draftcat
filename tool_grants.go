package main

// Standing grants: an operator's earlier word for a bounded run of calls.
//
// Approving the same routine call forty times a day teaches an operator to
// stop reading the prompt. A grant is the explicit alternative: the operator
// says once, on the operator channel, "send_email for the next hour, at most
// 20 calls", and the gate releases those calls on that word. Each release is
// a receipt that names the grant, the operator who issued it and the use it
// took. A grant never covers a high-risk tool, a call outside the rule's
// arguments or past a daily total — those still reach a human — and it stops
// applying the moment the rule no longer allows grants.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const (
	maxGrantDuration = 24 * time.Hour
	maxGrantUses     = 1000
)

// grantVerdict takes one use of a standing grant for this call, if a live one
// covers it, and returns the allow verdict and the granting operator.
func (g *toolGate) grantVerdict(req ToolCallRequest) (toolPolicyVerdict, int64, bool) {
	if state == nil {
		return toolPolicyVerdict{}, 0, false
	}
	gr, ok, err := state.UseToolGrant(req.Tool, req.Agent, g.now())
	if err != nil {
		log.Printf("[tool-gate] grant lookup for %s failed, asking a human: %v", req.Tool, err)
		return toolPolicyVerdict{}, 0, false
	}
	if !ok {
		return toolPolicyVerdict{}, 0, false
	}
	return toolPolicyVerdict{
		Decision: "allow", DecidedBy: "grant", Rule: "grant:" + gr.GrantID,
		Reason: fmt.Sprintf("standing grant %s by operator %d, use %d of %d", gr.GrantID, gr.GrantedBy, gr.Uses, gr.MaxUses),
	}, gr.GrantedBy, true
}

func newGrantID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("g_%x", time.Now().UnixNano())
	}
	return "g_" + hex.EncodeToString(b)
}

const grantUsage = "Usage: /grant <tool> <duration> [uses] [agent]\ne.g. /grant send_email 2h 20\n/grants lists live grants, /ungrant <id> ends one."

// parseGrant reads "/grant <tool> <duration> [uses] [agent]" and checks it
// against the tool's rule.
func parseGrant(args string, gate config.ToolGateConfig, by int64, now time.Time) (statestore.ToolGrant, error) {
	f := strings.Fields(args)
	if len(f) < 2 || len(f) > 4 {
		return statestore.ToolGrant{}, fmt.Errorf("%s", grantUsage)
	}
	rule, listed := gate.Lookup(f[0])
	switch {
	case !gate.Enabled || !listed:
		return statestore.ToolGrant{}, fmt.Errorf("%s is not in tool_gate.tools", f[0])
	case !rule.AllowGrants:
		return statestore.ToolGrant{}, fmt.Errorf("%s does not allow grants (allow_grants)", f[0])
	case !rule.Grantable():
		return statestore.ToolGrant{}, fmt.Errorf("%s is high risk — every call needs a human", f[0])
	}
	d, err := time.ParseDuration(f[1])
	if err != nil || d < time.Minute || d > maxGrantDuration {
		return statestore.ToolGrant{}, fmt.Errorf("duration must be between 1m and %s (got %q)", maxGrantDuration, f[1])
	}
	uses := maxGrantUses
	if len(f) >= 3 {
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 1 || n > maxGrantUses {
			return statestore.ToolGrant{}, fmt.Errorf("uses must be between 1 and %d (got %q)", maxGrantUses, f[2])
		}
		uses = n
	}
	agent := ""
	if len(f) == 4 {
		agent = f[3]
	}
	return statestore.ToolGrant{GrantID: newGrantID(), Tool: f[0], Agent: agent, GrantedBy: by,
		GrantedAt: now, ExpiresAt: now.Add(d).Truncate(time.Second), MaxUses: uses}, nil
}

func describeGrant(g statestore.ToolGrant, now time.Time) string {
	agent := "any agent"
	if g.Agent != "" {
		agent = "agent " + g.Agent
	}
	left := g.ExpiresAt.Sub(now).Round(time.Minute)
	return fmt.Sprintf("%s  %s, %s: %d of %d used, %s left (by %d)", g.GrantID, g.Tool, agent, g.Uses, g.MaxUses, left, g.GrantedBy)
}

// grantCommand answers /grant, /grants and /ungrant from operator `by`.
func grantCommand(cmd, args string, by int64, gate config.ToolGateConfig, now time.Time) string {
	if state == nil {
		return "Grants need the state store."
	}
	switch cmd {
	case "/grant":
		g, err := parseGrant(args, gate, by, now)
		if err != nil {
			return "Grant refused: " + err.Error()
		}
		if err := state.CreateToolGrant(g); err != nil {
			return "Grant not stored: " + err.Error()
		}
		log.Printf("[tool-gate] grant %s: %s until %s, %d use(s), agent=%q, by %d", g.GrantID, g.Tool, g.ExpiresAt.UTC().Format(time.RFC3339), g.MaxUses, g.Agent, by)
		return "Granted " + describeGrant(g, now) + "\n/ungrant " + g.GrantID + " ends it."
	case "/grants":
		list, err := state.ToolGrants(now, false)
		if err != nil {
			return "Grants unreadable: " + err.Error()
		}
		if len(list) == 0 {
			return "No live grants."
		}
		lines := []string{"Live grants:"}
		for _, g := range list {
			lines = append(lines, describeGrant(g, now))
		}
		return strings.Join(lines, "\n")
	case "/ungrant":
		id := strings.TrimSpace(args)
		if id == "" {
			return grantUsage
		}
		ok, err := state.RevokeToolGrant(id, by, now)
		switch {
		case err != nil:
			return "Grant not ended: " + err.Error()
		case !ok:
			return "No live grant " + id + "."
		}
		log.Printf("[tool-gate] grant %s ended by %d", id, by)
		return "Grant " + id + " ended."
	}
	return grantUsage
}
