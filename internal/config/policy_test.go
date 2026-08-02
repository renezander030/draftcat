package config

import "testing"

func step(name, risk string) StepConfig { return StepConfig{Name: name, Type: "approval", Risk: risk} }

func TestRiskOf_DefaultsToNormal(t *testing.T) {
	for _, in := range []string{"", "  ", "bogus", "Normal", "NORMAL"} {
		if got := (StepConfig{Risk: in}).RiskOf(); got != RiskNormal {
			t.Fatalf("RiskOf(%q) = %q, want normal", in, got)
		}
	}
	if got := (StepConfig{Risk: "LOW"}).RiskOf(); got != RiskLow {
		t.Fatalf("RiskOf(LOW) = %q, want low", got)
	}
	if got := (StepConfig{Risk: " High "}).RiskOf(); got != RiskHigh {
		t.Fatalf("RiskOf(' High ') = %q, want high", got)
	}
}

// The single most important property: a high-risk step is never auto-approved,
// no matter what a rule says.
func TestAutoApprove_HighRiskIsNeverExempt(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{
		{Risk: "high"}, {Risk: "low"}, {Risk: "normal"},
	}}
	if r := p.AutoApproves("inv", step("send", "high"), 0); r != nil {
		t.Fatalf("a high-risk step was auto-approved by rule %+v", *r)
	}
}

func TestAutoApprove_EmptyPolicyNeverExempts(t *testing.T) {
	var p ApprovalPolicy
	for _, risk := range []string{"", "low", "normal", "high"} {
		if r := p.AutoApproves("inv", step("send", risk), 0); r != nil {
			t.Fatalf("empty policy exempted a %q step", risk)
		}
	}
}

func TestAutoApprove_RiskMustMatch(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{{Risk: "low"}}}
	if p.AutoApproves("inv", step("send", "low"), 0) == nil {
		t.Fatal("a low-risk step was not matched by a low rule")
	}
	if r := p.AutoApproves("inv", step("send", "normal"), 0); r != nil {
		t.Fatalf("a normal step matched a low-only rule: %+v", *r)
	}
	// An unset step risk is normal, so it must not match a low rule.
	if r := p.AutoApproves("inv", step("send", ""), 0); r != nil {
		t.Fatalf("an unclassified step matched a low-only rule: %+v", *r)
	}
}

// A rule with no risk must never match — an unscoped exemption is the failure
// mode where a policy tier silently becomes "no gate".
func TestAutoApprove_RuleWithoutRiskNeverMatches(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{{Pipeline: "inv"}}}
	for _, risk := range []string{"low", "normal"} {
		if r := p.AutoApproves("inv", step("send", risk), 0); r != nil {
			t.Fatalf("a rule with no risk matched a %q step: %+v", risk, *r)
		}
	}
}

func TestAutoApprove_PipelineAndStepNarrowing(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{{Risk: "low", Pipeline: "inv", Step: "send"}}}
	if p.AutoApproves("inv", step("send", "low"), 0) == nil {
		t.Fatal("exact pipeline+step did not match")
	}
	if r := p.AutoApproves("other", step("send", "low"), 0); r != nil {
		t.Fatal("rule matched the wrong pipeline")
	}
	if r := p.AutoApproves("inv", step("archive", "low"), 0); r != nil {
		t.Fatal("rule matched the wrong step")
	}
}

// max_cost stops a cheap-per-action rule from quietly covering an expensive run.
func TestAutoApprove_MaxCostWithdrawsExemption(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{{Risk: "low", MaxCost: 1.0}}}
	if p.AutoApproves("inv", step("send", "low"), 0.50) == nil {
		t.Fatal("exemption withdrawn below max_cost")
	}
	if r := p.AutoApproves("inv", step("send", "low"), 1.0); r != nil {
		t.Fatal("exemption survived reaching max_cost")
	}
	if r := p.AutoApproves("inv", step("send", "low"), 2.5); r != nil {
		t.Fatal("exemption survived exceeding max_cost")
	}
}

func TestAutoApprove_FirstMatchWins(t *testing.T) {
	p := ApprovalPolicy{AutoApprove: []AutoApproveRule{
		{Risk: "low", Reason: "first"}, {Risk: "low", Reason: "second"},
	}}
	r := p.AutoApproves("inv", step("send", "low"), 0)
	if r == nil || r.Reason != "first" {
		t.Fatalf("got %+v, want the first matching rule", r)
	}
}

// The tool gate denies by default: an unlisted tool has no rule.
func TestToolGate_UnlistedToolHasNoRule(t *testing.T) {
	g := ToolGateConfig{Enabled: true, Tools: []ToolRule{{Name: "read_calendar"}}}
	if _, ok := g.Lookup("send_email"); ok {
		t.Fatal("an unlisted tool resolved to a rule")
	}
	if _, ok := g.Lookup("read_calendar"); !ok {
		t.Fatal("a listed tool did not resolve")
	}
}

func TestToolRule_RiskOfDefaultsToNormal(t *testing.T) {
	if got := (ToolRule{}).RiskOf(); got != RiskNormal {
		t.Fatalf("RiskOf() = %q, want normal", got)
	}
	if got := (ToolRule{Risk: "HIGH"}).RiskOf(); got != RiskHigh {
		t.Fatalf("RiskOf(HIGH) = %q, want high", got)
	}
}
