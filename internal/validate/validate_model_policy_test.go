package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestModelPolicyValidation(t *testing.T) {
	cfg := &config.Config{
		Roles: map[string]string{"drafter": "m"},
		ModelPolicy: config.ModelPolicyConfig{Rules: []config.ModelPolicyRule{
			{ID: "bad", Phase: "later", Pattern: "[", Action: "maybe", Roles: []string{"missing"}},
		}},
	}
	rep := &validateReport{}
	checkModelPolicy(cfg, rep)
	var findings []string
	for _, f := range rep.Findings {
		findings = append(findings, f.Path+": "+f.Message)
	}
	for _, suffix := range []string{".phase", ".pattern", ".action", ".roles"} {
		if !hasPath(findings, "model_policy.rules[0]"+suffix) {
			t.Fatalf("missing finding for %s: %+v", suffix, rep.Findings)
		}
	}
}
