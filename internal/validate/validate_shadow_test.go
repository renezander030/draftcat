package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func writeCandidate(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func shadowGate(path string) config.ToolGateConfig {
	return config.ToolGateConfig{Tools: []config.ToolRule{{Name: "read_file"}}, Shadow: &config.GateShadow{Config: path}}
}

func TestToolGateShadowValidCandidate(t *testing.T) {
	p := writeCandidate(t, "tool_gate:\n  tools:\n    - name: read_file\n      require_approval: true\n")
	errs, warns := gateFindings(t, shadowGate(p))
	if len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("errors=%v warnings=%v", errs, warns)
	}
	if f := CheckToolGateShadow(&config.Config{ToolGate: shadowGate(p)}); len(f) != 0 {
		t.Fatalf("CheckToolGateShadow: %+v", f)
	}
}

func TestToolGateShadowRefusesUnusableCandidates(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"empty path":      {"", "tool_gate.shadow.config"},
		"missing file":    {filepath.Join(dir, "nope.yaml"), "tool_gate.shadow.config"},
		"unparsable":      {writeCandidate(t, "tool_gate: [\n"), "tool_gate.shadow.config"},
		"no tool_gate":    {writeCandidate(t, "pipelines: []\n"), "has no tool_gate section"},
		"bad regex":       {writeCandidate(t, "tool_gate:\n  tools:\n    - name: t\n      args:\n        a: {regex: \"(\"}\n"), "tool_gate.shadow: tool_gate.tools[0].args.a.regex"},
		"high risk freed": {writeCandidate(t, "tool_gate:\n  tools:\n    - name: wire\n      risk: high\n"), "tool_gate.shadow: tool_gate.tools[0].require_approval"},
	} {
		errs, _ := gateFindings(t, shadowGate(c.path))
		if !anyHas(errs, c.want) {
			t.Errorf("%s: want an error at %q, got %v", name, c.want, errs)
		}
		if s := Summarize(CheckToolGateShadow(&config.Config{ToolGate: shadowGate(c.path)})); s.Errors == 0 {
			t.Errorf("%s: CheckToolGateShadow found no error", name)
		}
	}
}

func TestToolGateShadowIdenticalCandidateWarns(t *testing.T) {
	p := writeCandidate(t, "tool_gate:\n  enabled: false\n  tools:\n    - name: read_file\n")
	errs, warns := gateFindings(t, shadowGate(p))
	if len(errs) != 0 || !anyHas(warns, "same tool rules as the live gate") {
		t.Fatalf("errors=%v warnings=%v", errs, warns)
	}
}

func TestToolGateShadowOnADisabledGateWarns(t *testing.T) {
	rep := &validateReport{}
	checkToolGate(&config.Config{ToolGate: config.ToolGateConfig{Shadow: &config.GateShadow{Config: "x.yaml"}}}, rep)
	if len(rep.Findings) != 1 || rep.Findings[0].Level != "warn" || rep.Findings[0].Path != "tool_gate.shadow" {
		t.Fatalf("findings: %+v", rep.Findings)
	}
}
