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

func TestToolGate_ReviewAndDailyCapConstraints(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	errs, warns := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "create_invoice", OnMismatch: "deny",
		Args: map[string]config.ArgConstraint{"amount": {Min: v(0), Max: v(5000), ReviewAbove: v(500), MaxPerDay: v(10000)}},
	}}})
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("a well-formed band and cap produced findings: errors=%v warnings=%v", errs, warns)
	}
	errs, warns = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{
		Name: "refund", RequireApproval: true,
		Args: map[string]config.ArgConstraint{
			"amount": {Max: v(100), ReviewAbove: v(100)},
			"fee":    {MaxPerDay: v(-1)},
			"note":   {ReviewAbove: v(5)},
		},
	}}})
	if !anyHas(errs, "args.amount.review_above") || !anyHas(errs, "args.fee.max_per_day") {
		t.Fatalf("review_above at max and a negative cap must be errors: %v", errs)
	}
	if !anyHas(warns, "args.note.review_above") {
		t.Fatalf("review_above on a tool that always asks should warn: %v", warns)
	}
}

func TestToolGate_NotifyOutcomesValues(t *testing.T) {
	for _, ok := range []string{"", "off", "failed", "all"} {
		if errs, _ := gateFindings(t, config.ToolGateConfig{NotifyOutcomes: ok, Tools: []config.ToolRule{{Name: "t"}}}); anyHas(errs, "notify_outcomes") {
			t.Fatalf("%q refused: %v", ok, errs)
		}
	}
	if errs, _ := gateFindings(t, config.ToolGateConfig{NotifyOutcomes: "errors", Tools: []config.ToolRule{{Name: "t"}}}); !anyHas(errs, "tool_gate.notify_outcomes") {
		t.Fatalf("unknown mode accepted: %v", errs)
	}
}

func TestToolGate_CallCountCap(t *testing.T) {
	n := func(i int) *int { return &i }
	errs, warns := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", MaxCallsPerDay: n(50)}}})
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("a positive call cap produced findings: errors=%v warnings=%v", errs, warns)
	}
	errs, _ = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", MaxCallsPerDay: n(0)}}})
	if !anyHas(errs, "tools[0].max_calls_per_day") {
		t.Fatalf("a cap of 0 must be an error: %v", errs)
	}
	v := 1.0
	errs, _ = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "pay",
		Args: map[string]config.ArgConstraint{"#calls": {MaxPerDay: &v}}}}})
	if !anyHas(errs, "args.#calls") {
		t.Fatalf("a reserved argument name must be an error: %v", errs)
	}
}

func TestToolGate_Callers(t *testing.T) {
	t.Setenv("CALLER_A", "a")
	errs, warns := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email"}},
		Callers: []config.GateCaller{{Name: "a", TokenEnv: "CALLER_A", Tools: []string{"send_email"}}}})
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("a well-formed caller produced findings: errors=%v warnings=%v", errs, warns)
	}
	errs, warns = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email"}},
		Callers: []config.GateCaller{
			{Name: "a", TokenEnv: "CALLER_A", Tools: []string{"wire_money"}},
			{Name: "a", TokenEnv: "CALLER_A"},
			{Name: "", TokenEnv: ""},
			{Name: "c", TokenEnv: "CALLER_UNSET_FOR_TEST"},
		}})
	for _, want := range []string{"callers[1].name", "callers[1].token_env", "callers[2].name", "callers[2].token_env"} {
		if !anyHas(errs, want) {
			t.Fatalf("missing error %s: %v", want, errs)
		}
	}
	if !anyHas(warns, "callers[0].tools[0]") || !anyHas(warns, "callers[3].token_env") {
		t.Fatalf("unlisted caller tool and empty token should warn: %v", warns)
	}
	if errs, _ := gateFindings(t, config.ToolGateConfig{RequireCaller: true, Tools: []config.ToolRule{{Name: "t"}}}); !anyHas(errs, "require_caller") {
		t.Fatalf("require_caller without callers must be an error: %v", errs)
	}
}

func TestToolGate_AllowGrants(t *testing.T) {
	errs, warns := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", RequireApproval: true, AllowGrants: true}}})
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("a grantable approval tool produced findings: errors=%v warnings=%v", errs, warns)
	}
	errs, _ = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "wire", Risk: "high", RequireApproval: true, AllowGrants: true}}})
	if !anyHas(errs, "tools[0].allow_grants") {
		t.Fatalf("grants on a high-risk tool must be an error: %v", errs)
	}
	_, warns = gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "read", AllowGrants: true}}})
	if !anyHas(warns, "tools[0].allow_grants") {
		t.Fatalf("grants on a tool that never asks should warn: %v", warns)
	}
}

func TestToolGate_DefinitionPinForm(t *testing.T) {
	good := "sha256:" + strings.Repeat("a1", 32)
	for _, pin := range []string{good} {
		errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", DefinitionSHA256: pin}}})
		if len(errs) != 0 {
			t.Fatalf("%q: %v", pin, errs)
		}
	}
	for _, pin := range []string{"abc", "sha256:ABCD", good + "0", strings.TrimPrefix(good, "sha256:")} {
		errs, _ := gateFindings(t, config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", DefinitionSHA256: pin}}})
		if !anyHas(errs, "definition_sha256") {
			t.Fatalf("%q must be an error, got %v", pin, errs)
		}
	}
}
