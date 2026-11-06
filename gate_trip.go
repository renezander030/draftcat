package main

// Tool gate trip: the freeze an agent earns on its own.
//
// tool_gate.trip counts, per agent, the denials the gate hands out and, once
// an agent reaches the threshold inside the window, freezes that agent (or the
// whole gate). A looping agent is stopped before anyone reads the log. The
// agent freeze lives in the state store next to the gate freeze, so it
// survives a restart and a freeze written by a second shell reaches the
// running engine; /unfreeze <agent> and `draftcat gate unfreeze --agent` lift
// it.
//
// Counted: denials the rules or a human made about the call itself (not
// listed, outside the caller's tools, arguments or definition outside a deny
// rule, a daily total or call count past its cap, the repeat guard, an
// operator's refusal). Not counted: a frozen gate or agent (the trip must not
// feed itself), an approval that timed out or failed, a store or channel that
// was unavailable. Those say nothing about the agent.

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const (
	// tripBy is who a trip's freeze is recorded as set by.
	tripBy = "trip"
	// agentFreezeRule is the rule a frozen agent's calls are denied under.
	agentFreezeRule = "freeze.agent"
	// tripNoticeReasons is how many of the last denial reasons the trip
	// notice quotes.
	tripNoticeReasons = 3
)

// tripEvent is one counted denial.
type tripEvent struct {
	at           time.Time
	tool, reason string
}

// freezeAgentName is the name an agent is counted and frozen under: a call
// that names no agent counts as "-", the name the operator notices show.
func freezeAgentName(agent string) string { return orDash(agent) }

// agentFreezeReason is the reason a frozen agent's refused call carries.
func agentFreezeReason(f statestore.AgentFreeze) string {
	r := "agent " + f.Agent + " frozen"
	if f.By != "" {
		r += " by " + f.By
	}
	if f.Reason != "" {
		r += ": " + f.Reason
	}
	return r
}

// agentFreezeVerdict is the policy deny a frozen agent's calls get. Like the
// gate freeze it has no Class: no notice per call, and it does not count
// toward the trip.
func agentFreezeVerdict(reason string) toolPolicyVerdict {
	return toolPolicyVerdict{Decision: "deny", DecidedBy: "policy", Rule: agentFreezeRule, Reason: reason}
}

// agentFrozen reports whether the agent is frozen and the reason a refused
// call carries. The store is read on every decision; a store that cannot be
// read freezes.
func (g *toolGate) agentFrozen(agent string) (bool, string) {
	if state == nil {
		return false, ""
	}
	name := freezeAgentName(agent)
	f, ok, err := state.AgentFreeze(name)
	if err != nil {
		log.Printf("[tool-gate] agent freeze state unreadable, denying: %v", err)
		return true, "agent " + name + " frozen (freeze state could not be read)"
	}
	if !ok {
		return false, ""
	}
	return true, agentFreezeReason(f)
}

// describeAgentFreeze is one agent's status line.
func describeAgentFreeze(f statestore.AgentFreeze) string {
	line := "Agent " + f.Agent + ": FROZEN"
	if f.By != "" {
		line += " by " + f.By
	}
	line += " since " + f.At.UTC().Format(time.RFC3339)
	if f.Reason != "" {
		line += " (" + f.Reason + ")"
	}
	return line
}

// stillFrozenAgents is the note an unfreeze of the whole gate adds when
// agent freezes remain; empty when none do.
func stillFrozenAgents(st *statestore.StateStore, how string) string {
	agents, err := st.AgentFreezes()
	if err != nil || len(agents) == 0 {
		return ""
	}
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Agent
	}
	return "\nAgents still frozen: " + strings.Join(names, ", ") + " (" + how + " lifts one)."
}

// countDenial counts one denial of req's agent toward tool_gate.trip and
// trips once the agent reaches the threshold inside the window.
func (g *toolGate) countDenial(req ToolCallRequest, reason string) {
	t := g.cfg.ToolGate.Trip
	if t == nil {
		return
	}
	within := t.WithinDuration()
	if t.Denials < 2 || within <= 0 {
		return // validate refuses this; a trip that cannot be read never fires
	}
	now := g.now()
	agent := freezeAgentName(req.Agent)

	g.tripMu.Lock()
	if now.Sub(g.tripSwept) > within {
		for a, evs := range g.trips {
			if len(evs) == 0 || now.Sub(evs[len(evs)-1].at) > within {
				delete(g.trips, a)
			}
		}
		g.tripSwept = now
	}
	var evs []tripEvent
	for _, e := range g.trips[agent] {
		if now.Sub(e.at) <= within {
			evs = append(evs, e)
		}
	}
	evs = append(evs, tripEvent{at: now, tool: req.Tool, reason: reason})
	if len(evs) < t.Denials {
		g.trips[agent] = evs
		g.tripMu.Unlock()
		return
	}
	// Tripped: the count starts over, so a freeze that is lifted needs the
	// full threshold again.
	delete(g.trips, agent)
	g.tripMu.Unlock()
	g.trip(agent, evs, *t, now)
}

// trip freezes the agent, or the gate, and tells the operator once.
func (g *toolGate) trip(agent string, evs []tripEvent, t config.GateTrip, now time.Time) {
	window := strings.TrimSpace(t.Within)
	scope := t.ScopeOrDefault()
	var changed bool
	var err error
	switch {
	case state == nil:
		err = statestore.ErrNoStore
	case scope == "gate":
		changed, err = state.SetGateFreeze(fmt.Sprintf("agent %s was refused %d times within %s", agent, len(evs), window), tripBy, now)
	default:
		changed, err = state.SetAgentFreeze(agent, fmt.Sprintf("refused %d times within %s", len(evs), window), tripBy, now)
	}
	var head, lift string
	switch {
	case err != nil:
		log.Printf("[tool-gate] trip for agent %q fired but the freeze was not stored: %v", agent, err)
		head = fmt.Sprintf("[tool-gate] TRIP for agent %s (%d denials within %s) could not freeze: %v", agent, len(evs), window, err)
	case !changed:
		return // already frozen: the freeze and its notice are in place
	case scope == "gate":
		log.Printf("[tool-gate] TRIPPED: gate frozen after %d denials of agent %q within %s", len(evs), agent, window)
		head = fmt.Sprintf("[tool-gate] TRIPPED: tool gate frozen after %d denials of agent %s within %s", len(evs), agent, window)
		lift = "/unfreeze lifts it."
	default:
		log.Printf("[tool-gate] TRIPPED: agent %q frozen after %d denials within %s", agent, len(evs), window)
		head = fmt.Sprintf("[tool-gate] TRIPPED: agent %s frozen after %d denials within %s", agent, len(evs), window)
		lift = "/unfreeze " + agent + " lifts it."
	}
	if g.ch == nil {
		return
	}
	lines := []string{head, "last reasons:"}
	first := max(len(evs)-tripNoticeReasons, 0)
	for _, e := range evs[first:] {
		lines = append(lines, "  "+e.tool+": "+clipReason(e.reason))
	}
	if lift != "" {
		lines = append(lines, lift)
	}
	if err := g.ch.Send(strings.Join(lines, "\n")); err != nil {
		log.Printf("[tool-gate] trip notice failed: %v", err)
	}
}

func clipReason(s string) string {
	if r := []rune(s); len(r) > 160 {
		return string(r[:157]) + "..."
	}
	return s
}
