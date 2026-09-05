package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func gateFindings(t *testing.T, gate config.ToolGateConfig) (errs, warns []string) {
	t.Helper()
	cfg := &config.Config{Webhook: config.WebhookConfig{Enabled: true}, ToolGate: gate}
	cfg.ToolGate.Enabled = true
	rep := &validateReport{}
	checkToolGate(cfg, rep)
	for _, f := range rep.Findings {
		line := f.Path + ": " + f.Message
		if f.Level == "error" {
			errs = append(errs, line)
		} else {
			warns = append(warns, line)
		}
	}
	return errs, warns
}

func anyHas(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestToolGate_ValidConstraintsPass(t *testing.T) {
	max := 100.0
	errs, warns := gateFindings(t, config.ToolGateConfig{
		RepeatWindow: "15m", NotifyWindow: "0", MaxRepeats: 3,
		Tools: []config.ToolRule{{
			Name: "send_email", RequireApproval: true, OnMismatch: "deny",
			Args: map[string]config.ArgConstraint{
				"to":     {Glob: "*@example.com"},
				"amount": {Max: &max},
				"kind":   {OneOf: []string{"invoice", "reminder"}},
				"ref":    {Regex: `^INV-\d+$`, Optional: true},
			},
		}},
	})
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("a well-formed gate produced findings: errors=%v warnings=%v", errs, warns)
	}
}

func TestToolGate_BadRegexAndGlobAreErrors(t *testing.T) {
	errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "t", Args: map[string]config.ArgConstraint{"a": {Regex: "("}, "b": {Glob: "[unclosed"}},
	}}})
	if !anyHas(errs, "tool_gate.tools[0].args.a.regex") || !anyHas(errs, "tool_gate.tools[0].args.b.glob") {
		t.Fatalf("malformed patterns were accepted: %v", errs)
	}
}

func TestToolGate_EmptyConstraintIsAnError(t *testing.T) {
	errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "t", Args: map[string]config.ArgConstraint{"to": {}},
	}}})
	if !anyHas(errs, "tool_gate.tools[0].args.to: no condition") {
		t.Fatalf("an empty constraint was accepted: %v", errs)
	}
}

func TestToolGate_MinAboveMaxIsAnError(t *testing.T) {
	lo, hi := 10.0, 5.0
	errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "t", Args: map[string]config.ArgConstraint{"n": {Min: &lo, Max: &hi}},
	}}})
	if !anyHas(errs, "min 10 is greater than max 5") {
		t.Fatalf("min > max was accepted: %v", errs)
	}
}

func TestToolGate_OnMismatchMustBeApproveOrDeny(t *testing.T) {
	errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "t", OnMismatch: "ignore", Args: map[string]config.ArgConstraint{"a": {Glob: "*"}},
	}}})
	if !anyHas(errs, "tool_gate.tools[0].on_mismatch") {
		t.Fatalf("on_mismatch: ignore was accepted: %v", errs)
	}
}

func TestToolGate_RememberApprovalOnHighRiskIsAnError(t *testing.T) {
	errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "wire_money", Risk: "high", RequireApproval: true, RememberApproval: true,
	}}})
	if !anyHas(errs, "tool_gate.tools[0].remember_approval") {
		t.Fatalf("remembering approvals on a high-risk tool was accepted: %v", errs)
	}
}

func TestToolGate_RememberWithoutRequireIsAWarning(t *testing.T) {
	_, warns := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "t", RememberApproval: true}}})
	if !anyHas(warns, "no approval to remember") {
		t.Fatalf("pointless remember_approval went unmentioned: %v", warns)
	}
}

func TestToolGate_WindowsMustParse(t *testing.T) {
	errs, _ := gateFindings(t, config.ToolGateConfig{RepeatWindow: "soon", NotifyWindow: "-5m", MaxRepeats: -1, Tools: []config.ToolRule{{Name: "t"}}})
	for _, p := range []string{"tool_gate.repeat_window", "tool_gate.notify_window", "tool_gate.max_repeats"} {
		if !anyHas(errs, p) {
			t.Fatalf("%s accepted a bad value: %v", p, errs)
		}
	}
}
