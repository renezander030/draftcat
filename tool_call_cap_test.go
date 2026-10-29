package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

func intp(v int) *int { return &v }

func emailRule(onMismatch string, calls int) config.ToolRule {
	return config.ToolRule{Name: "send_email", OnMismatch: onMismatch, MaxCallsPerDay: intp(calls)}
}

func TestEvaluateToolPolicyCallCount(t *testing.T) {
	count := func(n int64) toolDailyTotal {
		return func(_, arg string) (*bigRat, error) {
			if arg != config.CallCountKey {
				t.Fatalf("unexpected total read for %q", arg)
			}
			return newRat(n), nil
		}
	}
	deny := config.ToolGateConfig{Tools: []config.ToolRule{emailRule("deny", 3)}}
	if v, _ := evaluateToolPolicy(deny, "send_email", nil, nil, count(2)); v.Decision != "allow" {
		t.Fatalf("the third call fits under 3: %+v", v)
	}
	v, err := evaluateToolPolicy(deny, "send_email", nil, nil, count(3))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "deny" || v.Rule != "max_calls_per_day" || v.Class != "cap" ||
		!strings.Contains(v.Reason, "calls: 3 already allowed today, max_calls_per_day 3") {
		t.Fatalf("the fourth call is refused: %+v", v)
	}
	ask := config.ToolGateConfig{Tools: []config.ToolRule{emailRule("", 3)}}
	v, _ = evaluateToolPolicy(ask, "send_email", nil, nil, count(3))
	if v.Decision != "ask" || len(v.Totals) != 1 || v.Totals[0] != "calls: 3 allowed today, max_calls_per_day 3" {
		t.Fatalf("an escalating rule asks with the count: %+v", v)
	}
}

// A rule with both kinds of cap reports both totals and names the cap passed.
func TestEvaluateToolPolicyCallCountWithArgumentCap(t *testing.T) {
	rule := invoiceRule("deny")
	rule.MaxCallsPerDay = intp(2)
	gate := config.ToolGateConfig{Tools: []config.ToolRule{rule}}
	spent := func(_, arg string) (*bigRat, error) {
		if arg == config.CallCountKey {
			return newRat(2), nil
		}
		return newRat(100), nil
	}
	v, _ := evaluateToolPolicy(gate, "create_invoice", map[string]interface{}{"amount": json.Number("50")}, nil, spent)
	if v.Decision != "deny" || v.Rule != "max_calls_per_day" || len(v.Totals) != 2 {
		t.Fatalf("count cap with an argument under its total: %+v", v)
	}
}

// Through the endpoint: allowed calls count, the call past the cap is refused,
// a revoked permit stops counting, and denied calls never counted.
func TestToolGateCallCountAccumulatesAcrossCalls(t *testing.T) {
	st := useReliabilityStore(t)
	cfg := gateCfg(emailRule("deny", 2))
	call := func(id, to string) ToolCallResponse {
		return callToolGate(t, cfg, nil, fmt.Sprintf(`{"action_id":%q,"tool":"send_email","args":{"to":%q}}`, id, to))
	}
	first := call("mail-1", "a@example.com")
	if first.Decision != "allow" || call("mail-2", "b@example.com").Decision != "allow" {
		t.Fatalf("two calls fit under 2: %+v", first)
	}
	r := call("mail-3", "c@example.com")
	if r.Decision != "deny" || r.Rule != "max_calls_per_day" {
		t.Fatalf("the third call is refused: %+v", r)
	}
	if r := call("mail-4", "d@example.com"); r.Decision != "deny" {
		t.Fatalf("a refused call does not free a slot: %+v", r)
	}
	if _, ok, err := st.CancelToolAction("mail-1", first.BindingHash, time.Now()); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if r := call("mail-5", "e@example.com"); r.Decision != "allow" {
		t.Fatalf("a revoked permit no longer counts: %+v", r)
	}
}

// A rule written without max_calls_per_day keeps its policy hash.
func TestToolPolicyHashIgnoresAbsentCallCap(t *testing.T) {
	rule := config.ToolRule{Name: "send_email", RequireApproval: true}
	before := hashToolPolicy("send_email", rule, true)
	rule.MaxCallsPerDay = intp(5)
	if hashToolPolicy("send_email", rule, true) == before {
		t.Fatal("max_calls_per_day must be part of the policy it binds")
	}
	rule.MaxCallsPerDay = nil
	if hashToolPolicy("send_email", rule, true) != before {
		t.Fatal("an absent max_calls_per_day must not change the hash")
	}
}

func TestGateCommandCalls(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `tool_gate:
  enabled: true
  tools:
    - name: send_email
      on_mismatch: deny
      max_calls_per_day: 20
`)
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"check", "send_email", "--calls", "20", "--config", cfgPath, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("check exit %d: %s", code, errOut.String())
	}
	var res gateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Decision != "deny" || res.Rule != "max_calls_per_day" {
		t.Fatalf("check --calls 20: %+v", res)
	}
	cases := filepath.Join(dir, "cases.yaml")
	writeFile(t, cases, `cases:
  - name: within the day
    tool: send_email
    calls: 19
    expect: allow
  - name: past the day
    tool: send_email
    calls: 20
    expect: deny
    rule: max_calls_per_day
`)
	out.Reset()
	if code := gateCmd([]string{"test", cases, "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("test exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if code := gateCmd([]string{"check", "send_email", "--calls", "-1", "--config", cfgPath}, &out, &errOut); code != 2 {
		t.Fatalf("a negative --calls is a usage error, got %d", code)
	}
}
