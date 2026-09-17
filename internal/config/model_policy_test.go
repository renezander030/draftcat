package config

import "testing"

func TestModelPolicyFirstMatchByPhaseAndRole(t *testing.T) {
	p := ModelPolicyConfig{Rules: []ModelPolicyRule{
		{ID: "input-secret", Phase: "input", Roles: []string{"drafter"}, Pattern: `(?i)secret`, Action: "review"},
		{ID: "output-id", Phase: "output", Pattern: `ID-[0-9]+`, Action: "deny"},
	}}
	r, err := p.Match("drafter", "input", "contains SECRET")
	if err != nil || r == nil || r.ID != "input-secret" {
		t.Fatalf("match=%+v err=%v", r, err)
	}
	if r, err = p.Match("classifier", "input", "contains secret"); err != nil || r != nil {
		t.Fatalf("role filter=%+v err=%v", r, err)
	}
	if r, err = p.Match("classifier", "output", "ID-42"); err != nil || r == nil || r.ID != "output-id" {
		t.Fatalf("output=%+v err=%v", r, err)
	}
}

func TestModelPolicyRejectsBadRegexAtRuntime(t *testing.T) {
	p := ModelPolicyConfig{Rules: []ModelPolicyRule{{ID: "bad", Phase: "both", Pattern: `[`, Action: "deny"}}}
	if _, err := p.Match("drafter", "input", "x"); err == nil {
		t.Fatal("bad regex was accepted")
	}
}
