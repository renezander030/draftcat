package main

// Tool gate freeze: one switch that stops every gated tool call.
//
// The freeze lives in the state store, so the Telegram commands, the
// `draftcat gate freeze` CLI and the running engine all see the same state,
// and it survives a restart. The engine reads it on every decision, so a
// freeze written from a second shell takes effect on the next call.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

const (
	freezeRule   = "freeze"
	freezePrefix = "tool gate frozen by operator"
)

const freezeUsage = "Usage: /freeze [reason] stops every gated tool call, /unfreeze lifts it, /unfreeze <agent> lifts one agent's freeze."

// freezeReason is the reason a refused call carries.
func freezeReason(f statestore.GateFreeze) string {
	r := freezePrefix
	if f.By != "" {
		r += " " + f.By
	}
	if f.Reason != "" {
		r += ": " + f.Reason
	}
	return r
}

// freezeVerdict is the policy deny a frozen gate answers with. It has no
// Class, so no operator notice is sent per denied call.
func freezeVerdict(reason string) toolPolicyVerdict {
	return toolPolicyVerdict{Decision: "deny", DecidedBy: "policy", Rule: freezeRule, Reason: reason}
}

// frozen reports whether the gate is frozen and the reason a refused call
// carries. The store is read on every decision. A store that cannot be read
// freezes: the gate denies rather than guesses.
func (g *toolGate) frozen() (bool, string) {
	if state == nil {
		return false, ""
	}
	f, err := state.GateFreeze()
	if err != nil {
		log.Printf("[tool-gate] freeze state unreadable, denying: %v", err)
		return true, freezePrefix + " (freeze state could not be read)"
	}
	if !f.Frozen {
		return false, ""
	}
	return true, freezeReason(f)
}

// describeFreeze is the status line for /status and `draftcat gate status`.
func describeFreeze(f statestore.GateFreeze) string {
	if !f.Frozen {
		return "Tool gate: not frozen"
	}
	line := "Tool gate: FROZEN"
	if f.By != "" {
		line += " by " + f.By
	}
	line += " since " + f.At.UTC().Format(time.RFC3339)
	if f.Reason != "" {
		line += " (" + f.Reason + ")"
	}
	return line
}

// freezeStatusLine is the extra /status text, only while the gate or an
// agent is frozen, so a gate that was never frozen reports exactly what it
// always did.
func freezeStatusLine() string {
	if state == nil {
		return ""
	}
	f, err := state.GateFreeze()
	if err != nil {
		return "Tool gate: freeze state unknown (" + err.Error() + ")"
	}
	var lines []string
	if f.Frozen {
		lines = append(lines, describeFreeze(f))
	}
	agents, err := state.AgentFreezes()
	if err != nil {
		lines = append(lines, "Agent freezes unknown ("+err.Error()+")")
	}
	for _, a := range agents {
		lines = append(lines, describeAgentFreeze(a))
	}
	return strings.Join(lines, "\n")
}

// freezeCommand answers /freeze and /unfreeze from operator `by`. allowed is
// the channel's allowed_users: the command checks the sender itself and does
// not rely on the caller having done so.
func freezeCommand(cmd, args string, by int64, allowed []int64, now time.Time) string {
	if !containsID(allowed, by) {
		log.Printf("[tool-gate] %s refused: %d is not an allowed operator", cmd, by)
		return "Not allowed: only operators on this channel can " + strings.TrimPrefix(cmd, "/") + " the tool gate."
	}
	if state == nil {
		return "Freeze needs the state store."
	}
	who := "telegram:" + strconv.FormatInt(by, 10)
	switch cmd {
	case "/freeze":
		changed, err := state.SetGateFreeze(strings.TrimSpace(args), who, now)
		if err != nil {
			return "Freeze not stored: " + err.Error()
		}
		if !changed {
			return "Already frozen.\n" + freezeStatusLine()
		}
		log.Printf("[tool-gate] FROZEN by %s: %s", who, strings.TrimSpace(args))
		return "Tool gate frozen: every gated call is denied. /unfreeze lifts it."
	case "/unfreeze":
		if agent := strings.TrimSpace(args); agent != "" {
			changed, err := state.ClearAgentFreeze(agent, who, now)
			if err != nil {
				return "Unfreeze not stored: " + err.Error()
			}
			if !changed {
				return "Agent " + agent + " was not frozen."
			}
			log.Printf("[tool-gate] agent %q UNFROZEN by %s", agent, who)
			return "Agent " + agent + " unfrozen."
		}
		changed, err := state.ClearGateFreeze(who, now)
		if err != nil {
			return "Unfreeze not stored: " + err.Error()
		}
		msg := "Tool gate unfrozen."
		if !changed {
			msg = "The tool gate was not frozen."
		} else {
			log.Printf("[tool-gate] UNFROZEN by %s", who)
		}
		return msg + stillFrozenAgents(state, "/unfreeze <agent>")
	}
	return freezeUsage
}

type gateStatusJSON struct {
	Frozen bool   `json:"frozen"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
	At     string `json:"at,omitempty"`
	// Agents lists the agents frozen on their own (tool_gate.trip).
	Agents []agentFreezeJSON `json:"agents,omitempty"`
}

type agentFreezeJSON struct {
	Agent  string `json:"agent"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
	At     string `json:"at"`
}

// gateFreezeCmd runs `draftcat gate freeze|unfreeze|status` on the state
// store of configPath. agent names the one agent unfreeze lifts.
func gateFreezeCmd(sub, configPath, reason, agent string, asJSON bool, stdout, stderr io.Writer) int {
	path := statePathFromConfigFile(configPath)
	if sub == "status" {
		// A status check never creates or migrates a store.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return printGateStatus(nil, asJSON, stdout, stderr)
		}
		st, err := statestore.OpenStateStoreReadOnly(path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		defer func() { _ = st.Close() }()
		return printGateStatus(st, asJSON, stdout, stderr)
	}
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	now := time.Now()
	switch sub {
	case "freeze":
		changed, err := st.SetGateFreeze(reason, "cli", now)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		if !changed {
			_, _ = fmt.Fprintln(stdout, "already frozen")
			return printGateStatus(st, false, stdout, stderr)
		}
		_, _ = fmt.Fprintln(stdout, "tool gate frozen")
	case "unfreeze":
		if agent != "" {
			changed, err := st.ClearAgentFreeze(agent, "cli", now)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
				return 1
			}
			if !changed {
				_, _ = fmt.Fprintf(stdout, "agent %s was not frozen\n", agent)
				return 0
			}
			_, _ = fmt.Fprintf(stdout, "agent %s unfrozen\n", agent)
			return 0
		}
		changed, err := st.ClearGateFreeze("cli", now)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		msg := "tool gate unfrozen"
		if !changed {
			msg = "tool gate was not frozen"
		}
		_, _ = fmt.Fprintln(stdout, msg+stillFrozenAgents(st, "gate unfreeze --agent <name>"))
	}
	return 0
}

func printGateStatus(st *statestore.StateStore, asJSON bool, stdout, stderr io.Writer) int {
	var f statestore.GateFreeze
	var agents []statestore.AgentFreeze
	if st != nil {
		var err error
		if f, err = st.GateFreeze(); err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		if agents, err = st.AgentFreezes(); err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
	}
	if asJSON {
		out := gateStatusJSON{Frozen: f.Frozen}
		if f.Frozen {
			out.Reason, out.By, out.At = f.Reason, f.By, f.At.UTC().Format(time.RFC3339)
		}
		for _, a := range agents {
			out.Agents = append(out.Agents, agentFreezeJSON{Agent: a.Agent, Reason: a.Reason, By: a.By, At: a.At.UTC().Format(time.RFC3339)})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}
	_, _ = fmt.Fprintln(stdout, describeFreeze(f))
	for _, a := range agents {
		_, _ = fmt.Fprintln(stdout, describeAgentFreeze(a))
	}
	return 0
}
