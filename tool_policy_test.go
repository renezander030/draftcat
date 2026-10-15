package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

func f64(v float64) *float64 { return &v }

type bigRat = big.Rat

func newRat(n int64) *big.Rat { return big.NewRat(n, 1) }

// recordingApprovalChannel answers every prompt with one decision and keeps
// the drafts it was shown.
type recordingApprovalChannel struct {
	stubApprovalChannel
	action string
	mu     sync.Mutex
	drafts []string
}

func (c *recordingApprovalChannel) SendForApproval(_ context.Context, d string, _ []int64) (OperatorDecision, error) {
	c.mu.Lock()
	c.drafts = append(c.drafts, d)
	c.mu.Unlock()
	return OperatorDecision{Action: c.action, ApproverID: 7}, nil
}

// A rule written before review_above and max_per_day existed must keep its
// policy hash, so permits bound before an upgrade still match after it.
func TestToolPolicyHashUnchangedForExistingRules(t *testing.T) {
	rule := config.ToolRule{Name: "create_invoice", Risk: "high", RequireApproval: true, OnMismatch: "deny",
		Args: map[string]config.ArgConstraint{
			"amount":   {Max: f64(500), Min: f64(1)},
			"customer": {OneOf: []string{"acme"}, Optional: true},
			"ch":       {Glob: "#ops-*", Regex: "^#", Equals: "x"},
		}}
	if got := hashToolPolicy("create_invoice", rule, true); got != "sha256:4ba802758219a7452c4239fc3e5a447c3e613e4adcfff6771fb948af4147870b" {
		t.Fatalf("policy hash changed: %s", got)
	}
	if got := hashToolPolicy("other", config.ToolRule{}, false); got != "sha256:ef2fea4d828b6cdf3b18255c7690e7fd5f62370d60a7685dda542bed1b33cd06" {
		t.Fatalf("unlisted policy hash changed: %s", got)
	}
}

func invoiceRule(onMismatch string) config.ToolRule {
	return config.ToolRule{Name: "create_invoice", OnMismatch: onMismatch,
		Args: map[string]config.ArgConstraint{"amount": {Min: f64(0), Max: f64(5000), ReviewAbove: f64(500), MaxPerDay: f64(1000)}}}
}

func TestEvaluateToolPolicyBands(t *testing.T) {
	gate := config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{invoiceRule("deny")}}
	zero := func(string, string) (*bigRat, error) { return newRat(0), nil }
	cases := []struct {
		amount   string
		decision string
		rule     string
	}{
		{"200", "allow", "listed"},
		{"500", "allow", "listed"},
		{"500.01", "ask", ""},
		{"5001", "deny", "args.amount: 5001 exceeds max 5000"},
		{"-1", "deny", "args.amount: -1 is below min 0"},
	}
	for _, c := range cases {
		v, err := evaluateToolPolicy(gate, "create_invoice", map[string]interface{}{"amount": json.Number(c.amount)}, zero)
		if err != nil {
			t.Fatal(err)
		}
		if v.Decision != c.decision || v.Rule != c.rule {
			t.Errorf("amount %s: got %s/%q, want %s/%q", c.amount, v.Decision, v.Rule, c.decision, c.rule)
		}
	}
	v, _ := evaluateToolPolicy(gate, "create_invoice", map[string]interface{}{"amount": json.Number("700")}, zero)
	if !strings.Contains(v.Why, "above the review threshold — amount: 700 is above review_above 500") {
		t.Fatalf("review reason missing: %q", v.Why)
	}
}

func TestEvaluateToolPolicyDailyCap(t *testing.T) {
	spent := func(string, string) (*bigRat, error) { return newRat(900), nil }
	deny := config.ToolGateConfig{Tools: []config.ToolRule{invoiceRule("deny")}}
	v, err := evaluateToolPolicy(deny, "create_invoice", map[string]interface{}{"amount": json.Number("200")}, spent)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "deny" || v.Rule != "max_per_day.amount" || v.Class != "cap" ||
		!strings.Contains(v.Reason, "today's total would be 1100, over max_per_day 1000") {
		t.Fatalf("over-cap deny rule: %+v", v)
	}
	ask := config.ToolGateConfig{Tools: []config.ToolRule{invoiceRule("")}}
	v, _ = evaluateToolPolicy(ask, "create_invoice", map[string]interface{}{"amount": json.Number("200")}, spent)
	if v.Decision != "ask" || len(v.Totals) != 1 || v.Totals[0] != "amount: 900 allowed today, max_per_day 1000" {
		t.Fatalf("over-cap approve rule should ask with the running total: %+v", v)
	}
	v, _ = evaluateToolPolicy(ask, "create_invoice", map[string]interface{}{"amount": json.Number("100")}, spent)
	if v.Decision != "allow" {
		t.Fatalf("a call that reaches the cap exactly is allowed: %+v", v)
	}
	failing := func(string, string) (*bigRat, error) { return nil, fmt.Errorf("disk gone") }
	if _, err := evaluateToolPolicy(ask, "create_invoice", map[string]interface{}{"amount": json.Number("1")}, failing); err == nil {
		t.Fatal("a total that cannot be read must be an error, not a guess")
	}
}

// Through the endpoint: allowed calls accumulate in the store, the call that
// would pass the cap is refused, a revoked permit stops counting, and the
// total survives a new gate instance (a restart).
func TestToolGateDailyCapAccumulatesAcrossCalls(t *testing.T) {
	st := useReliabilityStore(t)
	cfg := gateCfg(invoiceRule("deny"))
	call := func(id, amount string) ToolCallResponse {
		return callToolGate(t, cfg, nil, fmt.Sprintf(`{"action_id":%q,"tool":"create_invoice","args":{"amount":%s}}`, id, amount))
	}
	var second ToolCallResponse
	for i, amount := range []string{"400", "400"} {
		r := call(fmt.Sprintf("inv-%d", i), amount)
		if r.Decision != "allow" {
			t.Fatalf("call %d: %+v", i, r)
		}
		second = r
	}
	r := call("inv-3", "300")
	if r.Decision != "deny" || r.Rule != "max_per_day.amount" {
		t.Fatalf("third call should pass the cap and be denied: %+v", r)
	}
	if _, ok, err := st.CancelToolAction("inv-1", second.BindingHash, time.Now()); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if r := call("inv-4", "300"); r.Decision != "allow" {
		t.Fatalf("a revoked permit no longer counts toward the cap: %+v", r)
	}
	if r := call("inv-5", "300.5"); r.Decision != "deny" {
		t.Fatalf("400 + 300 allowed, 300.5 more is over 1000: %+v", r)
	}
}

func TestToolGateDailyCapNeedsTheStore(t *testing.T) {
	prev := state
	state = nil
	t.Cleanup(func() { state = prev })
	r := callToolGate(t, gateCfg(invoiceRule("")), nil, `{"tool":"create_invoice","args":{"amount":10}}`)
	if r.Decision != "deny" || r.Rule != "max_per_day" {
		t.Fatalf("without a readable total the gate denies: %+v", r)
	}
}

func TestToolGateReviewAbovePromptsWithReason(t *testing.T) {
	useReliabilityStore(t)
	ch := &recordingApprovalChannel{action: "approve"}
	cfg := gateCfg(invoiceRule(""))
	r := callToolGate(t, cfg, ch, `{"action_id":"inv-big","tool":"create_invoice","args":{"amount":800}}`)
	if r.Decision != "allow" || r.DecidedBy != "operator" {
		t.Fatalf("review band goes to the operator: %+v", r)
	}
	if len(ch.drafts) != 1 || !strings.Contains(ch.drafts[0], "rule:  above the review threshold — amount: 800 is above review_above 500") ||
		!strings.Contains(ch.drafts[0], "today: amount: 0 allowed today, max_per_day 1000") {
		t.Fatalf("prompt should say why and show the total: %q", ch.drafts)
	}
	// The operator-approved 800 counts: 300 more passes the cap.
	r = callToolGate(t, cfg, ch, `{"action_id":"inv-next","tool":"create_invoice","args":{"amount":300}}`)
	if r.DecidedBy != "operator" || len(ch.drafts) != 2 || !strings.Contains(ch.drafts[1], "daily total over the rule") {
		t.Fatalf("over-cap call should reach a human with the cap reason: %+v %q", r, ch.drafts)
	}
}

func TestGateCommandCheckAndTest(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `tool_gate:
  enabled: true
  tools:
    - name: create_invoice
      on_mismatch: deny
      args:
        amount: {min: 0, max: 5000, review_above: 500, max_per_day: 1000}
    - name: send_email
      require_approval: true
`)
	var out, errOut bytes.Buffer
	code := gateCmd([]string{"check", "create_invoice", "--args", `{"amount": 300}`, "--spent", "amount=800", "--config", cfgPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("check exit %d: %s", code, errOut.String())
	}
	var res gateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Decision != "deny" || res.Rule != "max_per_day.amount" {
		t.Fatalf("check: %+v", res)
	}

	cases := filepath.Join(dir, "cases.yaml")
	writeFile(t, cases, `cases:
  - name: small invoice
    tool: create_invoice
    args: {amount: 200}
    expect: allow
  - name: medium invoice
    tool: create_invoice
    args: {amount: 900}
    expect: ask
  - name: split past the cap
    tool: create_invoice
    args: {amount: 300}
    spent: {amount: "800"}
    expect: deny
    rule: max_per_day.amount
  - name: unlisted
    tool: wire_money
    expect: deny
    rule: not listed
  - name: email
    tool: send_email
    args: {to: a@b.c}
    expect: ask
`)
	out.Reset()
	if code := gateCmd([]string{"test", cases, "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("test exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "5 case(s), 0 failed") {
		t.Fatalf("summary: %s", out.String())
	}

	writeFile(t, cases, `cases:
  - name: wrong expectation
    tool: create_invoice
    args: {amount: 900}
    expect: allow
`)
	out.Reset()
	if code := gateCmd([]string{"test", cases, "--config", cfgPath}, &out, &errOut); code != 1 {
		t.Fatalf("a failing case must exit 1, got %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL  wrong expectation: ASK") || !strings.Contains(out.String(), "decided ask, expected allow") {
		t.Fatalf("failure output: %s", out.String())
	}
	errOut.Reset()
	if code := gateCmd([]string{"test", cases, "--config", cfgPath, "--spent", "amount=1"}, &out, &errOut); code != 2 {
		t.Fatalf("--spent with test is a usage error, got %d", code)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With several capped arguments, a refusing rule stops at the first cap passed
// and reports the totals read up to it; an escalating rule reads them all.
func TestEvaluateToolPolicySeveralCaps(t *testing.T) {
	rule := func(onMismatch string) config.ToolGateConfig {
		return config.ToolGateConfig{Tools: []config.ToolRule{{Name: "pay", OnMismatch: onMismatch, Args: map[string]config.ArgConstraint{
			"amount": {MaxPerDay: f64(100)}, "fee": {MaxPerDay: f64(10)}}}}}
	}
	reads := 0
	spent := func(_, arg string) (*bigRat, error) {
		reads++
		if arg == "amount" {
			return newRat(90), nil
		}
		return newRat(0), nil
	}
	args := map[string]interface{}{"amount": json.Number("20"), "fee": json.Number("1")}
	v, _ := evaluateToolPolicy(rule("deny"), "pay", args, spent)
	if v.Decision != "deny" || v.Rule != "max_per_day.amount" || len(v.Totals) != 1 || reads != 1 {
		t.Fatalf("deny stops at the first cap: %+v reads=%d", v, reads)
	}
	reads = 0
	v, _ = evaluateToolPolicy(rule(""), "pay", args, spent)
	if v.Decision != "ask" || len(v.Totals) != 2 || reads != 2 || !strings.Contains(v.Why, "amount: today's total would be 110") {
		t.Fatalf("escalation reads every cap: %+v reads=%d", v, reads)
	}
}
