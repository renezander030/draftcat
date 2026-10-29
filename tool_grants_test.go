package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

func grantableEmail() config.ToolRule {
	return config.ToolRule{Name: "send_email", RequireApproval: true, AllowGrants: true}
}

func TestStandingGrantReleasesBoundedCalls(t *testing.T) {
	st := useReliabilityStore(t)
	ch := &recordingApprovalChannel{action: "skip"}
	cfg := gateCfg(grantableEmail())
	if msg := grantCommand("/grant", "send_email 1h 2", 4242, cfg.ToolGate, time.Now()); !strings.HasPrefix(msg, "Granted g_") {
		t.Fatalf("grant: %s", msg)
	}
	g := newToolGate(cfg, ch)
	for i := 1; i <= 2; i++ {
		_, r := postGate(t, g, fmt.Sprintf(`{"action_id":"m-%d","tool":"send_email","args":{"n":%d}}`, i, i))
		if r.Decision != "allow" || r.DecidedBy != "grant" || !strings.HasPrefix(r.Rule, "grant:g_") ||
			!strings.Contains(r.Reason, fmt.Sprintf("by operator 4242, use %d of 2", i)) {
			t.Fatalf("call %d is released by the grant: %+v", i, r)
		}
	}
	if len(ch.drafts) != 0 {
		t.Fatalf("no prompt while the grant lasts: %q", ch.drafts)
	}
	_, r := postGate(t, g, `{"action_id":"m-3","tool":"send_email","args":{"n":3}}`)
	if r.DecidedBy != "operator" || len(ch.drafts) != 1 {
		t.Fatalf("a used-up grant hands back to the human: %+v", r)
	}
	recs, err := st.ApprovalsForPipeline("tool-gate", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range recs {
		if rec.ActionID == "m-1" {
			found = rec.OperatorID == 4242 && rec.Decision == "policy_approve" && strings.Contains(rec.Policy, "standing grant")
		}
	}
	if !found {
		t.Fatalf("the receipt names the granting operator and the grant: %+v", recs)
	}
}

// A grant covers only the plain require_approval ask: arguments outside the
// rule, a daily total passed or a review band still go to a human.
func TestStandingGrantDoesNotCoverEscalations(t *testing.T) {
	useReliabilityStore(t)
	ch := &recordingApprovalChannel{action: "skip"}
	rule := grantableEmail()
	rule.Args = map[string]config.ArgConstraint{"to": glob("*@example.com")}
	rule.MaxCallsPerDay = intp(1)
	cfg := gateCfg(rule)
	grantCommand("/grant", "send_email 1h 10", 1, cfg.ToolGate, time.Now())
	g := newToolGate(cfg, ch)
	if _, r := postGate(t, g, `{"action_id":"e-1","tool":"send_email","args":{"to":"anna@example.com"}}`); r.DecidedBy != "grant" {
		t.Fatalf("a matching call uses the grant: %+v", r)
	}
	if _, r := postGate(t, g, `{"action_id":"e-2","tool":"send_email","args":{"to":"anna@example.org"}}`); r.DecidedBy != "operator" {
		t.Fatalf("a call outside the arguments asks a human: %+v", r)
	}
	if _, r := postGate(t, g, `{"action_id":"e-3","tool":"send_email","args":{"to":"bob@example.com"}}`); r.DecidedBy != "operator" {
		t.Fatalf("a call past max_calls_per_day asks a human: %+v", r)
	}
	if len(ch.drafts) != 2 {
		t.Fatalf("two prompts expected, got %d", len(ch.drafts))
	}
}

func TestStandingGrantScopeAndRevocation(t *testing.T) {
	useReliabilityStore(t)
	ch := &recordingApprovalChannel{action: "skip"}
	cfg := gateCfg(grantableEmail())
	msg := grantCommand("/grant", "send_email 30m 5 inbox", 9, cfg.ToolGate, time.Now())
	id := strings.Fields(strings.TrimPrefix(msg, "Granted "))[0]
	g := newToolGate(cfg, ch)
	if _, r := postGate(t, g, `{"action_id":"s-1","tool":"send_email","agent":"billing","args":{}}`); r.DecidedBy != "operator" {
		t.Fatalf("a grant for one agent does not cover another: %+v", r)
	}
	if _, r := postGate(t, g, `{"action_id":"s-2","tool":"send_email","agent":"inbox","args":{}}`); r.DecidedBy != "grant" {
		t.Fatalf("the named agent is covered: %+v", r)
	}
	if !strings.Contains(grantCommand("/grants", "", 9, cfg.ToolGate, time.Now()), id) {
		t.Fatal("/grants lists the live grant")
	}
	if msg := grantCommand("/ungrant", id, 9, cfg.ToolGate, time.Now()); msg != "Grant "+id+" ended." {
		t.Fatalf("ungrant: %s", msg)
	}
	if _, r := postGate(t, g, `{"action_id":"s-3","tool":"send_email","agent":"inbox","args":{}}`); r.DecidedBy != "operator" {
		t.Fatalf("an ended grant releases nothing: %+v", r)
	}
	// The rule losing allow_grants ends what its grants release.
	grantCommand("/grant", "send_email 30m 5", 9, cfg.ToolGate, time.Now())
	off := gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true})
	if _, r := postGate(t, newToolGate(off, ch), `{"action_id":"s-4","tool":"send_email","args":{}}`); r.DecidedBy != "operator" {
		t.Fatalf("a rule without allow_grants ignores grants: %+v", r)
	}
}

func TestStandingGrantLastUseIsTakenOnce(t *testing.T) {
	useReliabilityStore(t)
	cfg := gateCfg(grantableEmail())
	grantCommand("/grant", "send_email 1h 3", 1, cfg.ToolGate, time.Now())
	g := newToolGate(cfg, &recordingApprovalChannel{action: "skip"})
	var mu sync.Mutex
	granted := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, r := postGate(t, g, fmt.Sprintf(`{"action_id":"c-%d","tool":"send_email","args":{"i":%d}}`, i, i))
			if r.DecidedBy == "grant" {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if granted != 3 {
		t.Fatalf("a 3-use grant released %d calls", granted)
	}
}

func TestGrantCommandRefusals(t *testing.T) {
	useReliabilityStore(t)
	high := config.ToolRule{Name: "wire", Risk: "high", RequireApproval: true, AllowGrants: true}
	plain := config.ToolRule{Name: "read", RequireApproval: true}
	gate := gateCfg(grantableEmail(), high, plain).ToolGate
	for args, want := range map[string]string{
		"":                       "Usage",
		"nope 1h":                "not in tool_gate.tools",
		"read 1h":                "does not allow grants",
		"wire 1h":                "high risk",
		"send_email 30s":         "duration must be between 1m",
		"send_email 25h":         "duration must be between 1m",
		"send_email 1h 0":        "uses must be between 1",
		"send_email 1h 1001":     "uses must be between 1",
		"send_email 1h 5 a b c ": "Usage",
	} {
		if got := grantCommand("/grant", args, 1, gate, time.Now()); !strings.Contains(got, want) {
			t.Errorf("/grant %q = %q, want %q", args, got, want)
		}
	}
	if got := grantCommand("/ungrant", "g_missing", 1, gate, time.Now()); got != "No live grant g_missing." {
		t.Errorf("ungrant unknown: %q", got)
	}
}

// The policy marks which asks a grant may answer (n: 9 passes its daily
// total of 5); the gate consults grants
// only for those.
func TestEvaluateToolPolicyMarksGrantableAsks(t *testing.T) {
	rule := grantableEmail()
	rule.Args = map[string]config.ArgConstraint{"to": glob("*@example.com"), "n": {MaxPerDay: f64(5), Optional: true}}
	gate := config.ToolGateConfig{Tools: []config.ToolRule{rule, {Name: "wire", Risk: "high", RequireApproval: true, AllowGrants: true}}}
	zero := func(string, string) (*bigRat, error) { return newRat(0), nil }
	for _, c := range []struct {
		tool string
		args map[string]interface{}
		want bool
	}{
		{"send_email", map[string]interface{}{"to": "a@example.com"}, true},
		{"send_email", map[string]interface{}{"to": "a@example.org"}, false},
		{"send_email", map[string]interface{}{"to": "a@example.com", "n": 9}, false},
		{"wire", nil, false},
	} {
		v, err := evaluateToolPolicy(gate, c.tool, c.args, nil, zero)
		if err != nil || v.Decision != "ask" || v.Grantable != c.want {
			t.Errorf("%s %v: decision %s grantable %v, want ask/%v (%v)", c.tool, c.args, v.Decision, v.Grantable, c.want, err)
		}
	}
}
