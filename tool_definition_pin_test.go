package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

const pinDefinition = `{"name":"send_email","description":"Send a message","inputSchema":{"type":"object","properties":{"to":{"type":"string"}}},"annotations":{"readOnlyHint":false}}`

// pinDefinitionReordered is pinDefinition with its keys in another order.
const pinDefinitionReordered = `{"annotations":{"readOnlyHint":false},"inputSchema":{"properties":{"to":{"type":"string"}},"type":"object"},"description":"Send a message","name":"send_email"}`

const pinDefinitionChanged = `{"name":"send_email","description":"Send a message and forward a copy","inputSchema":{"type":"object","properties":{"to":{"type":"string"}}},"annotations":{"readOnlyHint":false}}`

func pinOf(t *testing.T, definition string) string {
	t.Helper()
	var def map[string]interface{}
	if err := decodeStrictJSON([]byte(definition), &def); err != nil {
		t.Fatal(err)
	}
	return hashCanonical(def)
}

func pinnedGate(t *testing.T, onMismatch string) config.ToolGateConfig {
	t.Helper()
	return config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
		{Name: "send_email", OnMismatch: onMismatch, DefinitionSHA256: pinOf(t, pinDefinition)},
	}}
}

func decodeDefinition(t *testing.T, definition string) map[string]interface{} {
	t.Helper()
	var def map[string]interface{}
	if definition == "" {
		return nil
	}
	if err := decodeStrictJSON([]byte(definition), &def); err != nil {
		t.Fatal(err)
	}
	return def
}

func TestDefinitionPinDecisions(t *testing.T) {
	zero := func(string, string) (*bigRat, error) { return newRat(0), nil }
	cases := []struct {
		name       string
		onMismatch string
		definition string
		decision   string
		rule       string
		reason     string
	}{
		{"match is allowed", "deny", pinDefinition, "allow", "listed", ""},
		{"key order does not matter", "deny", pinDefinitionReordered, "allow", "listed", ""},
		{"changed definition is denied", "deny", pinDefinitionChanged, "deny", "definition", "definition outside the rule"},
		{"changed definition asks by default", "", pinDefinitionChanged, "ask", "", "definition outside the rule"},
		{"missing definition is denied", "deny", "", "deny", "definition", "no definition sent"},
		{"missing definition asks by default", "approve", "", "ask", "", "no definition sent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := evaluateToolPolicy(pinnedGate(t, c.onMismatch), "send_email", nil, decodeDefinition(t, c.definition), zero)
			if err != nil {
				t.Fatal(err)
			}
			if v.Decision != c.decision || v.Rule != c.rule {
				t.Fatalf("got %s via %q, want %s via %q (%+v)", v.Decision, v.Rule, c.decision, c.rule, v)
			}
			text := v.Reason + v.Why
			if !strings.Contains(text, c.reason) {
				t.Fatalf("reason %q does not contain %q", text, c.reason)
			}
		})
	}
}

// A rule without a pin ignores the definition: the decision is what it was
// without one.
func TestDefinitionIgnoredWithoutPin(t *testing.T) {
	zero := func(string, string) (*bigRat, error) { return newRat(0), nil }
	gate := config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{{Name: "send_email", OnMismatch: "deny"}}}
	without, _ := evaluateToolPolicy(gate, "send_email", nil, nil, zero)
	with, err := evaluateToolPolicy(gate, "send_email", nil, decodeDefinition(t, pinDefinitionChanged), zero)
	if err != nil {
		t.Fatal(err)
	}
	if with.Decision != "allow" || with.Rule != without.Rule || with.Reason != without.Reason {
		t.Fatalf("with %+v, without %+v", with, without)
	}
}

func TestDefinitionPinOverTheGateEndpoint(t *testing.T) {
	cfg := gateCfg(config.ToolRule{Name: "send_email", OnMismatch: "deny", DefinitionSHA256: pinOf(t, pinDefinition)})
	ch := &stubApprovalChannel{action: "skip"}
	ok := callToolGate(t, cfg, ch, `{"action_id":"a1","tool":"send_email","definition":`+pinDefinition+`}`)
	if ok.Decision != "allow" {
		t.Fatalf("matching definition: %+v", ok)
	}
	bad := callToolGate(t, cfg, ch, `{"action_id":"a2","tool":"send_email","definition":`+pinDefinitionChanged+`}`)
	if bad.Decision != "deny" || !strings.Contains(bad.Reason, "definition") {
		t.Fatalf("changed definition: %+v", bad)
	}
	none := callToolGate(t, cfg, ch, `{"action_id":"a3","tool":"send_email"}`)
	if none.Decision != "deny" || !strings.Contains(none.Reason, "no definition sent") {
		t.Fatalf("missing definition: %+v", none)
	}
}

// Sending a definition to an unpinned rule changes nothing in the response.
func TestDefinitionWithoutPinLeavesResponseUnchanged(t *testing.T) {
	cfg := gateCfg(config.ToolRule{Name: "read_calendar"})
	ch := &stubApprovalChannel{action: "skip"}
	plain := callToolGate(t, cfg, ch, `{"action_id":"a1","tool":"read_calendar"}`)
	sent := callToolGate(t, cfg, ch, `{"action_id":"a1","tool":"read_calendar","definition":`+pinDefinition+`}`)
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(sent)
	if !bytes.Equal(a, b) {
		t.Fatalf("response changed:\n%s\n%s", a, b)
	}
}

// A pin is part of the policy a permit binds; no pin leaves the hash alone.
func TestDefinitionPinPolicyHash(t *testing.T) {
	rule := config.ToolRule{Name: "send_email", RequireApproval: true}
	before := hashToolPolicy("send_email", rule, true)
	rule.DefinitionSHA256 = pinOf(t, pinDefinition)
	pinned := hashToolPolicy("send_email", rule, true)
	if pinned == before {
		t.Fatal("a pin must change the policy hash")
	}
	rule.DefinitionSHA256 = pinOf(t, pinDefinitionChanged)
	if hashToolPolicy("send_email", rule, true) == pinned {
		t.Fatal("a changed pin must change the policy hash")
	}
	rule.DefinitionSHA256 = ""
	if hashToolPolicy("send_email", rule, true) != before {
		t.Fatal("no pin must leave the policy hash unchanged")
	}
}

func TestGateHashDefinitionCommand(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	writeFile(t, a, pinDefinition)
	writeFile(t, b, pinDefinitionReordered)
	run := func(path string) string {
		var out, errOut bytes.Buffer
		if code := gateCmd([]string{"hash-definition", path}, &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		return strings.TrimSpace(out.String())
	}
	got := run(a)
	if got != run(b) || got != pinOf(t, pinDefinition) {
		t.Fatalf("hash not stable under key reordering: %s vs %s", got, run(b))
	}
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"hash-definition", filepath.Join(dir, "none.json")}, &out, &errOut); code != 2 {
		t.Fatalf("a missing file is a usage error, got %d", code)
	}
}

func TestGateCheckAndTestWithDefinition(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "tool_gate:\n  enabled: true\n  tools:\n    - name: send_email\n      on_mismatch: deny\n      definition_sha256: \""+pinOf(t, pinDefinition)+"\"\n")
	good, changed := filepath.Join(dir, "good.json"), filepath.Join(dir, "changed.json")
	writeFile(t, good, pinDefinitionReordered)
	writeFile(t, changed, pinDefinitionChanged)

	check := func(args ...string) gateResult {
		var out, errOut bytes.Buffer
		if code := gateCmd(append([]string{"check", "send_email", "--config", cfgPath, "--json"}, args...), &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		var r gateResult
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := check("--definition", good); r.Decision != "allow" {
		t.Fatalf("matching definition: %+v", r)
	}
	if r := check("--definition", changed); r.Decision != "deny" || r.Rule != "definition" {
		t.Fatalf("changed definition: %+v", r)
	}
	if r := check(); r.Decision != "deny" || r.Rule != "definition" {
		t.Fatalf("no definition: %+v", r)
	}

	cases := filepath.Join(dir, "cases.yaml")
	writeFile(t, cases, `cases:
  - name: pinned definition
    tool: send_email
    definition: {name: send_email, description: Send a message, inputSchema: {type: object, properties: {to: {type: string}}}, annotations: {readOnlyHint: false}}
    expect: allow
  - name: changed description
    tool: send_email
    definition: {name: send_email, description: Something else}
    expect: deny
    rule: definition
  - name: none sent
    tool: send_email
    expect: deny
    rule: definition
`)
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"test", cases, "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("test exit %d:\n%s%s", code, out.String(), errOut.String())
	}
}
