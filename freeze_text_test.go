package main

import (
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

// The freeze reason and status texts are operator-facing contracts
// (docs/tool-gate.md); this pins their exact shape for every by/reason mix.
func TestFreezeTextsExact(t *testing.T) {
	at := time.Date(2027, 1, 15, 8, 0, 0, 0, time.FixedZone("x", 3600))
	cases := []struct{ by, reason, gateReason, agentReason, gateLine, agentLine string }{
		{"", "", "tool gate frozen by operator", "agent a1 frozen",
			"Tool gate: FROZEN since 2027-01-15T07:00:00Z", "Agent a1: FROZEN since 2027-01-15T07:00:00Z"},
		{"telegram:1", "", "tool gate frozen by operator telegram:1", "agent a1 frozen by telegram:1",
			"Tool gate: FROZEN by telegram:1 since 2027-01-15T07:00:00Z", "Agent a1: FROZEN by telegram:1 since 2027-01-15T07:00:00Z"},
		{"", "why", "tool gate frozen by operator: why", "agent a1 frozen: why",
			"Tool gate: FROZEN since 2027-01-15T07:00:00Z (why)", "Agent a1: FROZEN since 2027-01-15T07:00:00Z (why)"},
		{"trip", "why", "tool gate frozen by operator trip: why", "agent a1 frozen by trip: why",
			"Tool gate: FROZEN by trip since 2027-01-15T07:00:00Z (why)", "Agent a1: FROZEN by trip since 2027-01-15T07:00:00Z (why)"},
	}
	for _, c := range cases {
		if got := freezeReasonText(freezePrefix, " ", c.by, c.reason); got != c.gateReason {
			t.Errorf("gate reason %q", got)
		}
		if got := freezeReasonText("agent a1 frozen", " by ", c.by, c.reason); got != c.agentReason {
			t.Errorf("agent reason %q", got)
		}
		if got := describeFreeze(statestore.GateFreeze{Frozen: true, By: c.by, At: at, Reason: c.reason}); got != c.gateLine {
			t.Errorf("gate line %q", got)
		}
		if got := describeAgentFreeze(statestore.AgentFreeze{Agent: "a1", By: c.by, At: at, Reason: c.reason}); got != c.agentLine {
			t.Errorf("agent line %q", got)
		}
	}
	if got := describeFreeze(statestore.GateFreeze{}); got != "Tool gate: not frozen" {
		t.Errorf("unfrozen %q", got)
	}
}
