package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/renezander030/draftcat/internal/config"
)

// pinToFile writes content as the rule's definition_file and loads it the way
// the engine does at start.
func pinToFile(t *testing.T, rule config.ToolRule, content string) config.ToolRule {
	t.Helper()
	rule.DefinitionFile = filepath.Join(t.TempDir(), rule.Name+".json")
	writeFile(t, rule.DefinitionFile, content)
	gate := config.ToolGateConfig{Tools: []config.ToolRule{rule}}
	if err := loadToolDefinitions(&gate); err != nil {
		t.Fatal(err)
	}
	return gate.Tools[0]
}

func zeroTotals(string, string) (*bigRat, error) { return newRat(0), nil }

func TestDefinitionChangesByKind(t *testing.T) {
	cases := []struct {
		name, pinned, sent string
		want               []string
	}{
		{"equal under key order", pinDefinition, pinDefinitionReordered, nil},
		{"long text shows an excerpt from the change", pinDefinition, pinDefinitionChanged,
			[]string{`description changed at char 15: " and forward a copy"`}},
		{"long text cut short", `{"description":"Send a message to the recipient"}`, `{"description":"Send a message"}`,
			[]string{`description cut to 14 chars (was 31)`}},
		{"short text shows both values", `{"title":"Send"}`, `{"title":"Send mail"}`,
			[]string{`title "Send" -> "Send mail"`}},
		{"fields added and removed", `{"name":"a","annotations":{"readOnlyHint":true}}`, `{"name":"a","title":"Send","outputSchema":{"type":"object"}}`,
			[]string{`annotations removed`, `outputSchema added`, `title added: "Send"`}},
		{"property added and required",
			`{"inputSchema":{"properties":{"to":{"type":"string"}},"required":["to"]}}`,
			`{"inputSchema":{"properties":{"to":{"type":"string"},"cc":{"type":"string"}},"required":["to","cc"]}}`,
			[]string{`inputSchema.properties.cc added`, `inputSchema.required +cc`}},
		{"property removed",
			`{"inputSchema":{"properties":{"to":{"type":"string"},"cc":{"type":"string"}},"required":["to","cc"]}}`,
			`{"inputSchema":{"properties":{"to":{"type":"string"}},"required":["to"]}}`,
			[]string{`inputSchema.properties.cc removed`, `inputSchema.required -cc`}},
		{"type changed", `{"inputSchema":{"properties":{"to":{"type":"string"}},"required":["to"]}}`, `{"inputSchema":{"properties":{"to":{"type":"array"}},"required":["to"]}}`,
			[]string{`inputSchema.properties.to.type "string" -> "array"`}},
		{"type widened to a list", `{"inputSchema":{"properties":{"to":{"type":"string"}}}}`, `{"inputSchema":{"properties":{"to":{"type":["string","null"]}}}}`,
			[]string{`inputSchema.properties.to.type "string" -> array`}},
		{"enum membership", `{"inputSchema":{"properties":{"p":{"enum":["low","high"]}}}}`, `{"inputSchema":{"properties":{"p":{"enum":["high","urgent",3]}}}}`,
			[]string{`inputSchema.properties.p.enum +urgent +3 -low`}},
		{"a list added", `{"inputSchema":{"properties":{"p":{}}}}`, `{"inputSchema":{"properties":{"p":{"enum":["a","b c","d","e","f"]}}}}`,
			[]string{`inputSchema.properties.p.enum added: [a, "b c", d, e, and 1 more]`}},
		{"members in another order", `{"inputSchema":{"required":["to","cc"]}}`, `{"inputSchema":{"required":["cc","to"]}}`,
			[]string{`inputSchema.required reordered`}},
		{"annotations", `{"annotations":{"readOnlyHint":true}}`, `{"annotations":{"readOnlyHint":false,"destructiveHint":true}}`,
			[]string{`annotations.destructiveHint added: true`, `annotations.readOnlyHint true -> false`}},
		{"numbers as written", `{"inputSchema":{"properties":{"n":{"maximum":100}}}}`, `{"inputSchema":{"properties":{"n":{"maximum":1000}}}}`,
			[]string{`inputSchema.properties.n.maximum 100 -> 1000`}},
		{"schemas in a list", `{"inputSchema":{"anyOf":[{"type":"string"},{"type":"number"}]}}`, `{"inputSchema":{"anyOf":[{"type":"string"},{"type":"integer"}]}}`,
			[]string{`inputSchema.anyOf[1].type "number" -> "integer"`}},
		{"a list of another length", `{"inputSchema":{"anyOf":[{"type":"string"}]}}`, `{"inputSchema":{"anyOf":[{"type":"string"},{"type":"null"}]}}`,
			[]string{`inputSchema.anyOf 1 -> 2 items`}},
		{"a key that is not a plain name", `{"inputSchema":{"properties":{}}}`, `{"inputSchema":{"properties":{"a b\nc":{}}}}`,
			[]string{`inputSchema.properties["a b\nc"] added`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := definitionChanges(decodeDefinition(t, c.pinned), decodeDefinition(t, c.sent))
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("changes:\n got  %q\n want %q", got, c.want)
			}
		})
	}
}

func TestDefinitionSummaryNamesTheChange(t *testing.T) {
	pinned := `{"name":"send_email","description":"Send a message","inputSchema":{"type":"object","properties":{"to":{"type":"string"}},"required":["to"]}}`
	sent := `{"name":"send_email","description":"Send a message and forward a copy","inputSchema":{"type":"object","properties":{"to":{"type":"string"},"cc":{"type":"string"}},"required":["to","cc"]}}`
	got := summarizeDefinitionChanges(definitionChanges(decodeDefinition(t, pinned), decodeDefinition(t, sent)))
	want := `definition changed: description changed at char 15: " and forward a copy"; inputSchema.properties.cc added; inputSchema.required +cc`
	if got != want {
		t.Fatalf("summary:\n got  %s\n want %s", got, want)
	}
}

func TestDefinitionSummaryIsBounded(t *testing.T) {
	props := map[string]interface{}{}
	for i := 0; i < 30; i++ {
		props[fmt.Sprintf("p%02d", i)] = map[string]interface{}{"type": "string"}
	}
	many := summarizeDefinitionChanges(definitionChanges(
		map[string]interface{}{"inputSchema": map[string]interface{}{"properties": map[string]interface{}{}}},
		map[string]interface{}{"inputSchema": map[string]interface{}{"properties": props}}))
	if n := strings.Count(many, "; "); n != maxDefinitionChanges || !strings.HasSuffix(many, "; and 22 more") {
		t.Fatalf("30 changes must show %d and count the rest: %s", maxDefinitionChanges, many)
	}

	long := strings.Repeat("Sends one message. ", 200)
	text := summarizeDefinitionChanges(definitionChanges(
		map[string]interface{}{"description": long},
		map[string]interface{}{"description": long + "Also forwards every message to an outside address."}))
	if strings.Contains(text, long) || utf8.RuneCountInString(text) > 120 || !strings.Contains(text, `"Also forwards every message to a"…`) {
		t.Fatalf("a long description must show only an excerpt: %s", text)
	}

	enum := []interface{}{}
	for i := 0; i < 20; i++ {
		enum = append(enum, fmt.Sprintf("v%d", i))
	}
	members := definitionChanges(map[string]interface{}{"enum": []interface{}{}}, map[string]interface{}{"enum": enum})
	if len(members) != 1 || members[0] != "enum +v0 +v1 +v2 +v3 and 16 more" {
		t.Fatalf("members must be capped: %q", members)
	}

	deep, deepSent := map[string]interface{}{}, map[string]interface{}{}
	key := strings.Repeat("k", 200)
	cur, curSent := deep, deepSent
	for i := 0; i < 50; i++ {
		next, nextSent := map[string]interface{}{}, map[string]interface{}{}
		cur[fmt.Sprintf("level_%02d_%s", i, strings.Repeat("x", 30))] = next
		curSent[fmt.Sprintf("level_%02d_%s", i, strings.Repeat("x", 30))] = nextSent
		cur, curSent = next, nextSent
	}
	curSent[key] = "line one\nline two"
	entries := definitionChanges(deep, deepSent)
	if len(entries) != 1 || utf8.RuneCountInString(entries[0]) > maxEntry || strings.Contains(entries[0], "\n") || strings.Contains(entries[0], key) {
		t.Fatalf("a deep or odd path must stay one bounded line: %q", entries)
	}
}

// A rule moved from definition_sha256 to a definition_file with the same
// content decides every call the same way.
func TestDefinitionFileDecidesLikeTheHashPin(t *testing.T) {
	for _, onMismatch := range []string{"deny", "approve", ""} {
		byHash := config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
			{Name: "send_email", OnMismatch: onMismatch, DefinitionSHA256: pinOf(t, pinDefinition)}}}
		byFile := config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{
			pinToFile(t, config.ToolRule{Name: "send_email", OnMismatch: onMismatch}, pinDefinitionReordered)}}
		for _, def := range []string{pinDefinition, pinDefinitionReordered, pinDefinitionChanged, `{"name":"send_email"}`, ""} {
			h, err := evaluateToolPolicy(byHash, "send_email", nil, decodeDefinition(t, def), zeroTotals)
			if err != nil {
				t.Fatal(err)
			}
			f, err := evaluateToolPolicy(byFile, "send_email", nil, decodeDefinition(t, def), zeroTotals)
			if err != nil {
				t.Fatal(err)
			}
			if h.Decision != f.Decision || h.Rule != f.Rule || h.DecidedBy != f.DecidedBy || h.Class != f.Class || h.Grantable != f.Grantable {
				t.Fatalf("on_mismatch %q, definition %s:\n hash %+v\n file %+v", onMismatch, def, h, f)
			}
		}
	}
}

// promptRecorder keeps the approval prompts it was sent.
type promptRecorder struct {
	stubApprovalChannel
	mu      sync.Mutex
	prompts []string
}

func (r *promptRecorder) SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, draft)
	r.mu.Unlock()
	return r.stubApprovalChannel.SendForApproval(ctx, draft, approvers)
}

func TestDefinitionFileReasonAndPrompt(t *testing.T) {
	const why = `definition outside the rule — definition changed: description changed at char 15: " and forward a copy"`

	deny := pinToFile(t, config.ToolRule{Name: "send_email", OnMismatch: "deny"}, pinDefinition)
	resp := callToolGate(t, gateCfg(deny), &stubApprovalChannel{action: "skip"},
		`{"action_id":"d1","tool":"send_email","definition":`+pinDefinitionChanged+`}`)
	if resp.Decision != "deny" || resp.Rule != "definition" || resp.Reason != why {
		t.Fatalf("deny: %+v", resp)
	}
	none := callToolGate(t, gateCfg(deny), &stubApprovalChannel{action: "skip"}, `{"action_id":"d2","tool":"send_email"}`)
	if none.Reason != "definition outside the rule — no definition sent, the rule pins the one in "+deny.DefinitionFile {
		t.Fatalf("missing definition: %+v", none)
	}

	ask := pinToFile(t, config.ToolRule{Name: "send_email"}, pinDefinition)
	ch := &promptRecorder{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	resp = callToolGate(t, gateCfg(ask), ch, `{"action_id":"a1","tool":"send_email","definition":`+pinDefinitionChanged+`}`)
	if resp.Decision != "allow" {
		t.Fatalf("approved ask: %+v", resp)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.prompts) != 1 || !strings.Contains(ch.prompts[0], "\nrule:  "+why+"\n") {
		t.Fatalf("prompt does not name the change:\n%s", strings.Join(ch.prompts, "\n---\n"))
	}

	// A rule whose file was never read fails closed.
	unread := config.ToolRule{Name: "send_email", OnMismatch: "deny", DefinitionFile: "send_email.json"}
	v, err := evaluateToolPolicy(config.ToolGateConfig{Tools: []config.ToolRule{unread}}, "send_email", nil, decodeDefinition(t, pinDefinition), zeroTotals)
	if err != nil || v.Decision != "deny" || !strings.Contains(v.Reason, "send_email.json is not loaded") {
		t.Fatalf("unread file: %+v %v", v, err)
	}
	if hashToolPolicy("send_email", unread, true) == hashToolPolicy("send_email", config.ToolRule{Name: "send_email", OnMismatch: "deny"}, true) {
		t.Fatal("an unread file pin must not hash like no pin")
	}
}

// A rule with only definition_sha256 keeps the reasons it gave before files
// existed.
func TestDefinitionHashPinReasonsUnchanged(t *testing.T) {
	pin := pinOf(t, pinDefinition)
	gate := config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", OnMismatch: "deny", DefinitionSHA256: pin}}}
	v, _ := evaluateToolPolicy(gate, "send_email", nil, decodeDefinition(t, pinDefinitionChanged), zeroTotals)
	if want := "definition outside the rule — the tool definition hashes to " + pinOf(t, pinDefinitionChanged) + ", the rule pins " + pin; v.Reason != want {
		t.Fatalf("changed:\n got  %s\n want %s", v.Reason, want)
	}
	v, _ = evaluateToolPolicy(gate, "send_email", nil, nil, zeroTotals)
	if want := "definition outside the rule — no definition sent, the rule pins " + pin; v.Reason != want {
		t.Fatalf("missing:\n got  %s\n want %s", v.Reason, want)
	}
}

func TestLoadToolDefinitions(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, pinDefinitionReordered)
	parsed := []config.ToolRule{{Name: "read_calendar"}, {Name: "send_email", DefinitionFile: good}}
	gate := config.ToolGateConfig{Tools: parsed}
	if err := loadToolDefinitions(&gate); err != nil {
		t.Fatal(err)
	}
	if p := gate.Tools[1].Pinned(); p == nil || p.SHA256 != pinOf(t, pinDefinition) {
		t.Fatalf("pin = %+v", p)
	}
	if parsed[1].Pinned() != nil {
		t.Fatal("loading must not touch the rules as parsed")
	}

	bad := map[string]string{"missing": filepath.Join(dir, "none.json")}
	for name, content := range map[string]string{"array": `[1]`, "empty": `{}`, "duplicate key": `{"a":1,"a":2}`, "trailing": `{"a":1} {}`} {
		bad[name] = filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		writeFile(t, bad[name], content)
	}
	bad["too large"] = filepath.Join(dir, "large.json")
	writeFile(t, bad["too large"], `{"description":"`+strings.Repeat("a", config.MaxToolDefinitionBytes)+`"}`)
	for name, path := range bad {
		gate := config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", DefinitionFile: path}}}
		if err := loadToolDefinitions(&gate); err == nil || !strings.Contains(err.Error(), "tool_gate.tools[0] (send_email).definition_file") {
			t.Errorf("%s: %v", name, err)
		}
	}
	both := config.ToolGateConfig{Tools: []config.ToolRule{{Name: "send_email", DefinitionFile: good, DefinitionSHA256: pinOf(t, pinDefinition)}}}
	if err := loadToolDefinitions(&both); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both pins: %v", err)
	}
}

func TestGateDiffDefinitionCommand(t *testing.T) {
	dir := t.TempDir()
	pinned, same, changed := filepath.Join(dir, "pinned.json"), filepath.Join(dir, "same.json"), filepath.Join(dir, "changed.json")
	const pinnedDef = `{"name":"send_email","description":"Send a message","inputSchema":{"type":"object","properties":{"to":{"type":"string"}},"required":["to"]}}`
	writeFile(t, pinned, pinnedDef)
	writeFile(t, same, `{"inputSchema":{"required":["to"],"properties":{"to":{"type":"string"}},"type":"object"},"description":"Send a message","name":"send_email"}`)
	writeFile(t, changed, `{"name":"send_email","description":"Send a message and forward a copy","inputSchema":{"type":"object","properties":{"to":{"type":"string"},"cc":{"type":"string"}},"required":["to","cc"]}}`)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := gateCmd(append([]string{"diff-definition"}, args...), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run(pinned, same); code != 0 || out != "definition unchanged: "+pinOf(t, pinnedDef)+"\n" {
		t.Fatalf("same definition: exit %d\n%s", code, out)
	}
	code, out, _ := run(pinned, changed)
	want := `definition changed: description changed at char 15: " and forward a copy"; inputSchema.properties.cc added; inputSchema.required +cc` + "\n"
	if code != 1 || !strings.HasPrefix(out, want) || !strings.Contains(out, "  pinned:  "+pinOf(t, pinnedDef)+"\n") {
		t.Fatalf("changed definition: exit %d\n%s", code, out)
	}
	code, out, _ = run(pinned, changed, "--json")
	var got struct {
		Changed bool     `json:"changed"`
		Pinned  string   `json:"pinned_sha256"`
		Current string   `json:"current_sha256"`
		Changes []string `json:"changes"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 1 || !got.Changed || len(got.Changes) != 3 || got.Pinned != pinOf(t, pinnedDef) {
		t.Fatalf("json: exit %d %+v %v\n%s", code, got, err, out)
	}
	if code, _, errOut := run(pinned, filepath.Join(dir, "none.json")); code != 2 || !strings.Contains(errOut, "none.json") {
		t.Fatalf("missing file: exit %d %s", code, errOut)
	}
	if code, _, _ := run(pinned); code != 2 {
		t.Fatalf("one file: exit %d", code)
	}
}

func TestGateCheckAndTestWithDefinitionFile(t *testing.T) {
	dir := t.TempDir()
	defPath := filepath.Join(dir, "send_email.json")
	writeFile(t, defPath, pinDefinition)
	changed := filepath.Join(dir, "changed.json")
	writeFile(t, changed, pinDefinitionChanged)
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "tool_gate:\n  enabled: true\n  tools:\n    - name: send_email\n      on_mismatch: deny\n      definition_file: "+defPath+"\n")

	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"check", "send_email", "--config", cfgPath, "--definition", changed}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `reason: definition outside the rule — definition changed: description changed at char 15: " and forward a copy"`) {
		t.Fatalf("check output:\n%s", out.String())
	}

	cases := filepath.Join(dir, "cases.yaml")
	writeFile(t, cases, `cases:
  - name: pinned definition
    tool: send_email
    definition: {name: send_email, description: Send a message, inputSchema: {type: object, properties: {to: {type: string}}}, annotations: {readOnlyHint: false}}
    expect: allow
  - name: cc added
    tool: send_email
    definition: {name: send_email, description: Send a message, inputSchema: {type: object, properties: {to: {type: string}, cc: {type: string}}}, annotations: {readOnlyHint: false}}
    expect: deny
    rule: definition
`)
	out.Reset()
	errOut.Reset()
	if code := gateCmd([]string{"test", cases, "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("test exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "definition changed: inputSchema.properties.cc added") {
		t.Fatalf("test output does not name the change:\n%s", out.String())
	}

	// A file the gate cannot read refuses the config, like a parse error.
	writeFile(t, cfgPath, "tool_gate:\n  enabled: true\n  tools:\n    - name: send_email\n      definition_file: "+filepath.Join(dir, "none.json")+"\n")
	out.Reset()
	errOut.Reset()
	if code := gateCmd([]string{"check", "send_email", "--config", cfgPath}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "definition_file") {
		t.Fatalf("missing file: exit %d %s", code, errOut.String())
	}
}

func TestReloadRereadsDefinitionFile(t *testing.T) {
	resetLive(t)
	t.Setenv("RL_TG", "x")
	t.Setenv("RL_KEY", "y")
	t.Setenv("RL_HOOK", "z")
	dir := t.TempDir()
	defPath := filepath.Join(dir, "send_email.json")
	writeFile(t, defPath, pinDefinition)
	body := reloadBase + "webhook:\n  enabled: true\n  secret_env: RL_HOOK\ntool_gate:\n  enabled: true\n  tools:\n    - name: send_email\n      on_mismatch: deny\n      definition_file: " + defPath + "\n"
	running, boot := bootFrom(t, body)
	if err := loadToolDefinitions(&running.ToolGate); err != nil {
		t.Fatal(err)
	}
	path := writeReloadConfig(t, dir, body)
	sched := newScheduler(running.Pipelines)
	gate := newToolGate(running, nil)
	decide := func(def string) toolPolicyVerdict {
		t.Helper()
		v, err := evaluateToolPolicy(gate.cfg.ToolGate, "send_email", nil, decodeDefinition(t, def), zeroTotals)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	policy := func() string {
		rule, listed := gate.cfg.ToolGate.Lookup("send_email")
		return gate.policyHash("send_email", rule, listed)
	}
	before := policy()
	if decide(pinDefinition).Decision != "allow" || decide(pinDefinitionChanged).Decision != "deny" {
		t.Fatal("boot pin not in force")
	}

	writeFile(t, defPath, pinDefinitionChanged)
	msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Reloaded") || !strings.Contains(msg, "Tool definitions re-read and changed: send_email") || strings.Contains(msg, "restart") {
		t.Fatalf("reload summary: %q", msg)
	}
	if decide(pinDefinitionChanged).Decision != "allow" {
		t.Fatal("the running gate still decides on the old definition")
	}
	if v := decide(pinDefinition); v.Decision != "deny" || !strings.Contains(v.Reason, "description cut to 14 chars (was 33)") {
		t.Fatalf("old definition after reload: %+v", v)
	}
	after := policy()
	if after == before || after != hashToolPolicyGate(gate, pinOf(t, pinDefinitionChanged)) {
		t.Fatal("the policy hash must follow the re-read pin")
	}
	if msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil); strings.Contains(msg, "Tool definitions") {
		t.Fatalf("an unchanged file is not a change: %q", msg)
	}

	for name, broken := range map[string]func(){
		"unparsable": func() { writeFile(t, defPath, "{") },
		"missing":    func() { _ = os.Remove(defPath) },
	} {
		broken()
		msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil)
		if !strings.Contains(msg, "Reload refused") || !strings.Contains(msg, "definition_file") {
			t.Fatalf("%s file: %q", name, msg)
		}
		if decide(pinDefinitionChanged).Decision != "allow" || policy() != after {
			t.Fatalf("%s file: a refused reload changed the pin", name)
		}
	}
}

// hashToolPolicyGate is the gate's policy hash for the send_email rule pinned
// by hash, for comparing with a file pin.
func hashToolPolicyGate(g *toolGate, pin string) string {
	return g.policyHash("send_email", config.ToolRule{Name: "send_email", OnMismatch: "deny", DefinitionSHA256: pin}, true)
}
