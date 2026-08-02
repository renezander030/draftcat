package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func policyFindings(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	rep := &validateReport{}
	checkApprovalPolicy(cfg, rep)
	checkToolGate(cfg, rep)
	var out []string
	for _, f := range rep.Findings {
		if f.Level == "error" {
			out = append(out, f.Path+": "+f.Message)
		}
	}
	return out
}

// The failure this guards: a rule with no risk scope matches every non-high
// step everywhere, which is "no gate at all" written to look like governance.
func TestPolicy_RuleWithoutRiskIsAnError(t *testing.T) {
	cfg := &config.Config{Policy: config.ApprovalPolicy{
		AutoApprove: []config.AutoApproveRule{{Pipeline: "inv"}},
	}}
	if f := policyFindings(t, cfg); !hasPath(f, "approval_policy.auto_approve[0].risk") {
		t.Fatalf("an unscoped auto-approve rule was accepted: %v", f)
	}
}

func TestPolicy_HighRiskRuleIsAnError(t *testing.T) {
	cfg := &config.Config{Policy: config.ApprovalPolicy{
		AutoApprove: []config.AutoApproveRule{{Risk: "high"}},
	}}
	if f := policyFindings(t, cfg); !hasPath(f, "approval_policy.auto_approve[0].risk") {
		t.Fatalf("a high-risk auto-approve rule was accepted: %v", f)
	}
}

func TestPolicy_UnknownRiskIsAnError(t *testing.T) {
	cfg := &config.Config{Policy: config.ApprovalPolicy{
		AutoApprove: []config.AutoApproveRule{{Risk: "medium"}},
	}}
	if f := policyFindings(t, cfg); !hasPath(f, "approval_policy.auto_approve[0].risk") {
		t.Fatalf("an unknown risk level was accepted: %v", f)
	}
}

func TestPolicy_RuleNamingUnknownPipelineIsAnError(t *testing.T) {
	cfg := &config.Config{Policy: config.ApprovalPolicy{
		AutoApprove: []config.AutoApproveRule{{Risk: "low", Pipeline: "nope"}},
	}}
	if f := policyFindings(t, cfg); !hasPath(f, "approval_policy.auto_approve[0].pipeline") {
		t.Fatalf("a rule naming a nonexistent pipeline was accepted: %v", f)
	}
}

func TestPolicy_ValidLowRiskRulePasses(t *testing.T) {
	cfg := &config.Config{
		Pipelines: []config.PipelineConfig{{Name: "inv"}},
		Policy: config.ApprovalPolicy{AutoApprove: []config.AutoApproveRule{
			{Risk: "low", Pipeline: "inv", Reason: "internal drafts only"},
		}},
	}
	if f := policyFindings(t, cfg); len(f) != 0 {
		t.Fatalf("a valid rule produced errors: %v", f)
	}
}

func TestPolicy_StepRiskMustBeKnown(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name:  "inv",
		Steps: []config.StepConfig{{Name: "send", Type: "approval", Risk: "critical"}},
	}}}
	if f := policyFindings(t, cfg); !hasPath(f, "pipelines.inv.steps.send.risk") {
		t.Fatalf("an unknown step risk was accepted: %v", f)
	}
}

// --- tool gate ---

func TestToolGate_RequiresWebhookListener(t *testing.T) {
	cfg := &config.Config{ToolGate: config.ToolGateConfig{
		Enabled: true, Tools: []config.ToolRule{{Name: "x"}},
	}}
	if f := policyFindings(t, cfg); !hasPath(f, "tool_gate.enabled") {
		t.Fatalf("tool gate was accepted with no webhook listener: %v", f)
	}
}

// A high-risk tool anyone can call without a human is the exact gap the tool
// gate exists to close, so the config that expresses it must not validate.
func TestToolGate_HighRiskToolWithoutApprovalIsAnError(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Enabled: true},
		ToolGate: config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
			{Name: "send_email", Risk: "high", RequireApproval: false},
		}},
	}
	if f := policyFindings(t, cfg); !hasPath(f, "tool_gate.tools[0].require_approval") {
		t.Fatalf("a high-risk tool with no approval was accepted: %v", f)
	}
}

func TestToolGate_DuplicateToolIsAnError(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Enabled: true},
		ToolGate: config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
			{Name: "send_email", RequireApproval: true},
			{Name: "send_email", RequireApproval: false},
		}},
	}
	if f := policyFindings(t, cfg); !hasPath(f, "tool_gate.tools[1].name") {
		t.Fatalf("a duplicate tool entry was accepted: %v", f)
	}
}

func TestToolGate_ValidConfigPasses(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Enabled: true},
		ToolGate: config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
			{Name: "read_calendar", Risk: "low"},
			{Name: "send_email", Risk: "high", RequireApproval: true},
		}},
	}
	if f := policyFindings(t, cfg); len(f) != 0 {
		t.Fatalf("a valid tool gate produced errors: %v", f)
	}
}
