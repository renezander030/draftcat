package main

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

// windowGateCfg reads a tool_gate config the way the engine does, so a key
// the config does not know is ignored here as it would be there.
func windowGateCfg(t *testing.T, src string) *config.Config {
	t.Helper()
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

// A rule's approval_window replaces timeouts.operator_approval for that
// tool's calls, shorter or longer; a rule without one keeps the global.
func TestToolGate_RuleApprovalWindow(t *testing.T) {
	cfg := windowGateCfg(t, `
timeouts: {operator_approval: 2s}
tool_gate:
  enabled: true
  tools:
    - {name: restart_worker, require_approval: true, approval_window: 200ms}
    - {name: deploy, require_approval: true, approval_window: 3s}
    - {name: send_email, require_approval: true}
`)
	// Shorter: a human who never answers is a timeout deny once the rule's
	// 200ms have passed, not the global 2s.
	start := time.Now()
	resp := callToolGate(t, cfg, &stubApprovalChannel{action: "approve", delay: 5 * time.Second}, `{"tool":"restart_worker"}`)
	if took := time.Since(start); resp.Decision != "deny" || took > 1500*time.Millisecond {
		t.Fatalf("restart_worker: %q after %v, want a timeout deny inside its 200ms window", resp.Decision, took)
	}

	// Longer: with the global window at 150ms, an approval after one second
	// is inside deploy's 3s window and outside send_email's global one.
	cfg.Timeouts.OperatorApproval = "150ms"
	resp = callToolGate(t, cfg, &stubApprovalChannel{action: "approve", id: 7, delay: time.Second}, `{"tool":"deploy"}`)
	if resp.Decision != "allow" || resp.DecidedBy != "operator" {
		t.Fatalf("deploy: got %+v, want the operator's allow inside its 3s window", resp)
	}
	resp = callToolGate(t, cfg, &stubApprovalChannel{action: "approve", id: 7, delay: time.Second}, `{"tool":"send_email"}`)
	if resp.Decision != "deny" {
		t.Fatalf("send_email: got %+v, want a timeout deny in the global 150ms window", resp)
	}

	// The window a call was decided under is part of its policy hash.
	cfg.Timeouts.OperatorApproval = "3s"
	g := newToolGate(cfg, nil)
	hash := func(rule string) string {
		var r config.ToolRule
		if err := yaml.Unmarshal([]byte(rule), &r); err != nil {
			t.Fatal(err)
		}
		return g.policyHash(r.Name, r, true)
	}
	plain := hash(`{name: deploy, require_approval: true}`)
	if hash(`{name: deploy, require_approval: true, approval_window: 30m}`) == plain {
		t.Fatal("a rule's approval_window does not change the policy hash")
	}
	if hash(`{name: deploy, require_approval: true, approval_window: 3s}`) != plain {
		t.Fatal("an approval_window equal to the global window changed the policy hash")
	}
}
