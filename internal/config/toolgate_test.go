package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func jsonArgs(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("args %q: %v", s, err)
	}
	return m
}

func TestArgConstraint_Check(t *testing.T) {
	max, min := 100.0, 1.0
	cases := []struct {
		name string
		c    ArgConstraint
		v    interface{}
		ok   bool
		why  string
	}{
		{"glob match", ArgConstraint{Glob: "*@example.com"}, "anna@example.com", true, ""},
		{"glob miss", ArgConstraint{Glob: "*@example.com"}, "mallory@evil.example", false, "does not match glob"},
		{"regex match", ArgConstraint{Regex: `^INV-\d+$`}, "INV-42", true, ""},
		{"regex miss", ArgConstraint{Regex: `^INV-\d+$`}, "INV-x", false, "does not match regex"},
		{"one_of hit", ArgConstraint{OneOf: []string{"a", "b"}}, "b", true, ""},
		{"one_of miss", ArgConstraint{OneOf: []string{"a", "b"}}, "c", false, "is not one of"},
		{"equals string", ArgConstraint{Equals: "x"}, "x", true, ""},
		{"equals number vs json float", ArgConstraint{Equals: 5}, 5.0, true, ""},
		{"equals bool", ArgConstraint{Equals: true}, true, true, ""},
		{"equals miss", ArgConstraint{Equals: "x"}, "y", false, "is not"},
		{"max ok", ArgConstraint{Max: &max}, 99.5, true, ""},
		{"max exceeded", ArgConstraint{Max: &max}, 100.5, false, "exceeds max 100"},
		{"min below", ArgConstraint{Min: &min}, 0.0, false, "below min 1"},
		{"numeric string", ArgConstraint{Max: &max}, "50", true, ""},
		{"not a number", ArgConstraint{Max: &max}, "lots", false, "is not a number"},
		{"all conditions must hold", ArgConstraint{Glob: "*@example.com", Regex: "^anna"}, "bob@example.com", false, "does not match regex"},
	}
	for _, tc := range cases {
		ok, why := tc.c.Check(tc.v, true)
		if ok != tc.ok || (tc.why != "" && !strings.Contains(why, tc.why)) {
			t.Errorf("%s: Check(%v) = (%v, %q), want (%v, ~%q)", tc.name, tc.v, ok, why, tc.ok, tc.why)
		}
	}
}

func TestArgConstraint_MissingArgument(t *testing.T) {
	if ok, why := (ArgConstraint{Glob: "*"}).Check(nil, false); ok || why != "missing" {
		t.Fatalf("absent required argument = (%v, %q), want (false, missing)", ok, why)
	}
	if ok, _ := (ArgConstraint{Glob: "*", Optional: true}).Check(nil, false); !ok {
		t.Fatal("absent optional argument must pass")
	}
}

func TestToolRule_MatchArgsNamesTheFirstFailureInKeyOrder(t *testing.T) {
	r := ToolRule{Args: map[string]ArgConstraint{
		"to":      {Glob: "*@example.com"},
		"subject": {Regex: "^Re:"},
	}}
	ok, why := r.MatchArgs(jsonArgs(t, `{"to":"x@evil.example","subject":"spam"}`))
	if ok || !strings.HasPrefix(why, "subject:") {
		t.Fatalf("MatchArgs = (%v, %q), want the first failing key in sorted order (subject before to)", ok, why)
	}
	if ok, _ := r.MatchArgs(jsonArgs(t, `{"to":"anna@example.com","subject":"Re: invoice"}`)); !ok {
		t.Fatal("a call satisfying every constraint must match")
	}
	if ok, _ := (ToolRule{}).MatchArgs(nil); !ok {
		t.Fatal("a rule without constraints matches everything")
	}
}

func TestToolRule_DeniesOnMismatch(t *testing.T) {
	if (ToolRule{}).DeniesOnMismatch() || (ToolRule{OnMismatch: "approve"}).DeniesOnMismatch() {
		t.Fatal("default on_mismatch must be approve (ask a human)")
	}
	if !(ToolRule{OnMismatch: " Deny "}).DeniesOnMismatch() {
		t.Fatal("on_mismatch: deny not recognised")
	}
}

func TestArgString(t *testing.T) {
	for v, want := range map[interface{}]string{"s": "s", 3.0: "3", 2.5: "2.5", true: "true", 7: "7", nil: ""} {
		if got := ArgString(v); got != want {
			t.Errorf("ArgString(%v) = %q, want %q", v, got, want)
		}
	}
	if got := ArgString([]interface{}{"a", 1.0}); got != `["a",1]` {
		t.Errorf("ArgString(list) = %q, want compact JSON", got)
	}
}

func TestToolGateConfig_Defaults(t *testing.T) {
	g := ToolGateConfig{}
	if !g.NotifyDenialsOn() {
		t.Fatal("notify_denials must default to on")
	}
	if g.RepeatWindowOrDefault() != 10*time.Minute || g.NotifyWindowOrDefault() != 10*time.Minute {
		t.Fatal("windows must default to 10m")
	}
	g = ToolGateConfig{RepeatWindow: "0", NotifyWindow: "2h"}
	if g.RepeatWindowOrDefault() != 0 || g.NotifyWindowOrDefault() != 2*time.Hour {
		t.Fatalf("windows = %s / %s, want 0 / 2h", g.RepeatWindowOrDefault(), g.NotifyWindowOrDefault())
	}
	off := false
	if (ToolGateConfig{NotifyDenials: &off}).NotifyDenialsOn() {
		t.Fatal("notify_denials: false not honoured")
	}
}

func TestProviderConfig_UsageAccounting(t *testing.T) {
	if !(ProviderConfig{}).UsageAccountingOn() || !(ProviderConfig{Type: "OpenRouter"}).UsageAccountingOn() {
		t.Fatal("usage accounting must default on for OpenRouter (including the implicit empty type)")
	}
	if (ProviderConfig{Type: "openai"}).UsageAccountingOn() {
		t.Fatal("usage accounting must default off for other providers")
	}
	on := true
	if !(ProviderConfig{Type: "openai", UsageAccounting: &on}).UsageAccountingOn() {
		t.Fatal("explicit usage_accounting: true not honoured")
	}
	if (ProviderConfig{}).RetriesOrDefault() != 3 || (ProviderConfig{MaxRetries: 1}).RetriesOrDefault() != 1 {
		t.Fatal("retry budget default/override wrong")
	}
}
