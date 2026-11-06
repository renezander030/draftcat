package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestToolGateTripValid(t *testing.T) {
	for _, trip := range []config.GateTrip{
		{Denials: 5, Within: "10m"},
		{Denials: 2, Within: "30s", Scope: "agent"},
		{Denials: 3, Within: "1h", Scope: "Gate"},
	} {
		trip := trip
		errs, warns := gateFindings(t, config.ToolGateConfig{Trip: &trip, Tools: []config.ToolRule{{Name: "read_file"}}})
		if len(errs) != 0 || len(warns) != 0 {
			t.Fatalf("%+v: errors=%v warnings=%v", trip, errs, warns)
		}
	}
}

func TestToolGateTripRefusesBadSettings(t *testing.T) {
	for _, c := range []struct {
		trip config.GateTrip
		path string
	}{
		{config.GateTrip{Denials: 1, Within: "10m"}, "tool_gate.trip.denials"},
		{config.GateTrip{Denials: 0, Within: "10m"}, "tool_gate.trip.denials"},
		{config.GateTrip{Denials: 5}, "tool_gate.trip.within"},
		{config.GateTrip{Denials: 5, Within: "0"}, "tool_gate.trip.within"},
		{config.GateTrip{Denials: 5, Within: "-1m"}, "tool_gate.trip.within"},
		{config.GateTrip{Denials: 5, Within: "ten minutes"}, "tool_gate.trip.within"},
		{config.GateTrip{Denials: 5, Within: "10m", Scope: "tool"}, "tool_gate.trip.scope"},
	} {
		trip := c.trip
		errs, _ := gateFindings(t, config.ToolGateConfig{Trip: &trip, Tools: []config.ToolRule{{Name: "read_file"}}})
		if !anyHas(errs, c.path) {
			t.Errorf("%+v: want an error at %s, got %v", c.trip, c.path, errs)
		}
	}
}

func TestToolGateTripOnADisabledGateWarns(t *testing.T) {
	rep := &validateReport{}
	checkToolGate(&config.Config{ToolGate: config.ToolGateConfig{Trip: &config.GateTrip{Denials: 5, Within: "10m"}}}, rep)
	if len(rep.Findings) != 1 || rep.Findings[0].Level != "warn" || rep.Findings[0].Path != "tool_gate.trip" {
		t.Fatalf("findings: %+v", rep.Findings)
	}
}
