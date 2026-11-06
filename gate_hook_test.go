package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

const hookTestToken = "hook-secret"

// claudePayload is a Claude Code PreToolUse payload. The 1.50 keeps its
// spelling all the way into the gate's argument hash.
func claudePayload(tool, toolUseID, input string) string {
	return `{"session_id":"sess-1","transcript_path":"/tmp/t.jsonl","cwd":"/work","permission_mode":"default",` +
		`"hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":` + input + `,"tool_use_id":"` + toolUseID + `"}`
}

func TestHookPayload_Claude(t *testing.T) {
	c, err := parseHookPayload("claude", []byte(claudePayload("Bash", "toolu_1", `{"command":"ls","timeout":1.50}`)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tool != "Bash" || c.SessionID != "sess-1" || c.ToolUseID != "toolu_1" || c.Cwd != "/work" || c.Event != "PreToolUse" {
		t.Fatalf("fields: %+v", c)
	}
	if n, ok := c.Args["timeout"].(json.Number); !ok || n.String() != "1.50" {
		t.Fatalf("number spelling lost: %#v", c.Args["timeout"])
	}
	other, _ := parseHookPayload("claude", []byte(claudePayload("Bash", "toolu_2", `{"command":"ls","timeout":1.50}`)))
	if hookActionID("claude", c) == hookActionID("claude", other) {
		t.Fatal("two tool uses share an action id")
	}
	again, _ := parseHookPayload("claude", []byte(claudePayload("Bash", "toolu_1", `{"command":"ls","timeout":1.50}`)))
	id := hookActionID("claude", c)
	if id != hookActionID("claude", again) || !validActionID(id) {
		t.Fatalf("action id not stable or not valid: %q", id)
	}
}

func TestHookPayload_Codex(t *testing.T) {
	raw := `{"session_id":"s-9","turn_id":"turn-1","transcript_path":null,"cwd":"/w","hook_event_name":"PreToolUse",` +
		`"model":"m","permission_mode":"default","tool_name":"mcp__github__create_issue","tool_input":{"title":"t","labels":["a"]},"tool_use_id":"call_7"}`
	c, err := parseHookPayload("codex", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tool != "mcp__github__create_issue" || c.ToolUseID != "call_7" || c.SessionID != "s-9" || c.Args["title"] != "t" {
		t.Fatalf("fields: %+v", c)
	}
}

func TestHookPayload_CopilotBothFormats(t *testing.T) {
	camel := `{"sessionId":"cs","timestamp":1704614600000,"cwd":"/w","toolName":"bash","toolArgs":"{\"command\":\"ls\",\"n\":1.50}"}`
	c, err := parseHookPayload("copilot", []byte(camel))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tool != "bash" || c.SessionID != "cs" || c.Timestamp != "1704614600000" || c.Args["command"] != "ls" {
		t.Fatalf("camelCase fields: %+v", c)
	}
	if n, _ := c.Args["n"].(json.Number); n.String() != "1.50" {
		t.Fatalf("toolArgs number spelling lost: %#v", c.Args["n"])
	}
	pascal := `{"hook_event_name":"PreToolUse","session_id":"cs","timestamp":"2026-01-01T00:00:00Z","cwd":"/w","tool_name":"Bash","tool_input":{"command":"ls"}}`
	p, err := parseHookPayload("copilot", []byte(pascal))
	if err != nil {
		t.Fatal(err)
	}
	if p.Tool != "Bash" || p.Timestamp != "2026-01-01T00:00:00Z" || p.Args["command"] != "ls" {
		t.Fatalf("PreToolUse fields: %+v", p)
	}
	if _, err := parseHookPayload("copilot", []byte(`{"toolName":"bash","toolArgs":"not json"}`)); err == nil {
		t.Fatal("unparseable toolArgs must be refused")
	}
}

func TestHookPayload_GeminiIdentifiedByItsFields(t *testing.T) {
	raw := func(ts string) []byte {
		return []byte(`{"session_id":"g1","transcript_path":"/t","cwd":"/w","hook_event_name":"BeforeTool","timestamp":"` + ts +
			`","tool_name":"run_shell_command","tool_input":{"command":"ls"}}`)
	}
	a, err := parseHookPayload("gemini", raw("2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := parseHookPayload("gemini", raw("2026-01-01T00:00:00Z"))
	later, _ := parseHookPayload("gemini", raw("2026-01-01T00:00:05Z"))
	if a.Tool != "run_shell_command" || a.ToolUseID != "" {
		t.Fatalf("fields: %+v", a)
	}
	if hookActionID("gemini", a) != hookActionID("gemini", b) {
		t.Fatal("the same payload must give the same action id")
	}
	if hookActionID("gemini", a) == hookActionID("gemini", later) {
		t.Fatal("a later call must give a new action id")
	}
}

func TestHookPayload_RefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"not json":       `ls -la`,
		"array":          `[1,2]`,
		"null":           `null`,
		"no tool":        `{"session_id":"s","tool_input":{}}`,
		"other event":    `{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{}}`,
		"input array":    `{"tool_name":"Bash","tool_input":["ls"]}`,
		"input string":   `{"tool_name":"Bash","tool_input":"ls"}`,
		"duplicate keys": `{"tool_name":"Bash","tool_name":"Write","tool_input":{}}`,
	}
	for name, raw := range cases {
		if _, err := parseHookPayload("claude", []byte(raw)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	if _, err := parseHookPayload("gemini", []byte(`{"hook_event_name":"AfterTool","tool_name":"x"}`)); err == nil {
		t.Error("gemini AfterTool: parsed, want an error")
	}
}

func TestHookAnswer_PerFormat(t *testing.T) {
	allow, deny := hookVerdict{Allow: true, Reason: "ok"}, hookVerdict{Reason: "nope"}
	decode := func(format string, v hookVerdict) map[string]interface{} {
		t.Helper()
		var b bytes.Buffer
		if err := writeHookAnswer(&b, format, v, "Bash"); err != nil {
			t.Fatal(err)
		}
		if b.Len() == 0 {
			return nil
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v (%q)", format, err, b.String())
		}
		return m
	}
	specific := func(m map[string]interface{}) map[string]interface{} {
		s, _ := m["hookSpecificOutput"].(map[string]interface{})
		return s
	}

	for _, f := range []string{"claude", "copilot"} {
		a := specific(decode(f, allow))
		if a["hookEventName"] != "PreToolUse" || a["permissionDecision"] != "allow" {
			t.Errorf("%s allow: %v", f, a)
		}
		d := specific(decode(f, deny))
		if d["permissionDecision"] != "deny" || !strings.Contains(d["permissionDecisionReason"].(string), "nope") {
			t.Errorf("%s deny: %v", f, d)
		}
	}
	if top := decode("copilot", deny); top["permissionDecision"] != "deny" || !strings.Contains(top["permissionDecisionReason"].(string), "nope") {
		t.Errorf("copilot deny needs the top-level fields too: %v", top)
	}
	if m := decode("codex", allow); m != nil {
		t.Errorf("codex allow must answer nothing, got %v", m)
	}
	if d := specific(decode("codex", deny)); d["permissionDecision"] != "deny" || d["permissionDecisionReason"] == "" {
		t.Errorf("codex deny: %v", d)
	}
	if m := decode("gemini", allow); m["decision"] != "allow" {
		t.Errorf("gemini allow: %v", m)
	}
	if m := decode("gemini", deny); m["decision"] != "deny" || !strings.Contains(m["reason"].(string), "nope") {
		t.Errorf("gemini deny: %v", m)
	}
}

// realHookGate serves the real gate behind the authenticator the engine
// mounts, with signatures required, on a local port.
func realHookGate(t *testing.T, cfg *config.Config, ch OperatorChannel) *httptest.Server {
	t.Helper()
	useReliabilityStore(t)
	cfg.Timeouts.OperatorApproval = "1m"
	gate := newToolGate(cfg, ch)
	auth := newGateAuthenticator([]byte(hookTestToken), cfg.ToolGate).middleware(1<<20, 300, true)
	mux := http.NewServeMux()
	mux.Handle(toolGatePath, auth(http.HandlerFunc(gate.HandleCall)))
	mux.Handle(toolGatePath+"/", auth(http.HandlerFunc(gate.HandleStatus)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runHookCmd runs `draftcat gate hook` with a payload and decodes a Claude
// answer: allowed, and the reason.
func runHookCmd(t *testing.T, payload string, args ...string) (bool, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := gateHookCmd(append([]string{"--format", "claude"}, args...), strings.NewReader(payload), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var ans struct {
		Out struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &ans); err != nil {
		t.Fatalf("answer %q: %v", out.String(), err)
	}
	if ans.Out.Decision == "deny" && !strings.Contains(errb.String(), "blocked") {
		t.Errorf("a block names its reason on stderr, got %q", errb.String())
	}
	return ans.Out.Decision == "allow", ans.Out.Reason
}

func TestGateHook_AllowAsksThenConsumes(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	srv := realHookGate(t, gateCfg(config.ToolRule{Name: "Bash"}), nil)
	payload := claudePayload("Bash", "toolu_1", `{"command":"ls","timeout":1.50}`)
	ok, reason := runHookCmd(t, payload, "--url", srv.URL)
	if !ok {
		t.Fatalf("listed tool was not allowed: %s", reason)
	}
	call, _ := parseHookPayload("claude", []byte(payload))
	a, err := state.ToolAction(hookActionID("claude", call))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "consumed" || a.Agent != "claude-code" || a.RunID != "sess-1" || a.Tool != "Bash" {
		t.Fatalf("action: %+v", a)
	}
	want := hashToolArgs(map[string]interface{}{"command": "ls", "timeout": json.Number("1.50")})
	if a.ArgsHash != want {
		t.Fatalf("args hash %s, want the hash of the input as written %s", a.ArgsHash, want)
	}

	// The same tool use again joins the decision already spent: one decision,
	// one execution.
	if ok, reason := runHookCmd(t, payload, "--url", srv.URL); ok || !strings.Contains(reason, "already used") {
		t.Fatalf("a replayed hook must be blocked, got allow=%v %q", ok, reason)
	}
}

func TestGateHook_OperatorApprovalReachesTheHarness(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	srv := realHookGate(t, gateCfg(config.ToolRule{Name: "Write", RequireApproval: true}),
		&stubApprovalChannel{action: "approve", id: 7, delay: 200 * time.Millisecond})
	if ok, reason := runHookCmd(t, claudePayload("Write", "toolu_w", `{"file_path":"a.txt","content":"x"}`), "--url", srv.URL); !ok {
		t.Fatalf("approved call was not allowed: %s", reason)
	}
}

func TestGateHook_DenyCarriesTheGateReason(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	srv := realHookGate(t, gateCfg(config.ToolRule{Name: "Bash"}), nil)
	ok, reason := runHookCmd(t, claudePayload("mcp__mail__send_email", "toolu_2", `{"to":"anna@example.com"}`), "--url", srv.URL)
	if ok || !strings.Contains(reason, "tool_gate.tools") {
		t.Fatalf("unlisted tool: allow=%v %q", ok, reason)
	}
}

func TestGateHook_BadTokenBlocks(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, "not-the-token")
	srv := realHookGate(t, gateCfg(config.ToolRule{Name: "Bash"}), nil)
	ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_3", `{"command":"ls"}`), "--url", srv.URL)
	if ok || !strings.Contains(reason, "refused the token from "+hookDefaultTokenEnv) {
		t.Fatalf("bad token: allow=%v %q", ok, reason)
	}
}

func TestGateHook_NoTokenBlocksWithoutAsking(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, "")
	t.Setenv(hookFallbackTokenEnv, "")
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_4", `{"command":"ls"}`), "--url", srv.URL)
	if ok || hit || !strings.Contains(reason, "no gate token") {
		t.Fatalf("no token: allow=%v hit=%v %q", ok, hit, reason)
	}
}

func TestGateHook_CallerTokenNeedsTheCallerName(t *testing.T) {
	t.Setenv("GATE_TOKEN_AGENT1", "agent-1-token")
	t.Setenv("HOOK_TOKEN", "agent-1-token")
	cfg := gateCfg(config.ToolRule{Name: "Bash"})
	cfg.ToolGate.Callers = []config.GateCaller{{Name: "agent-1", TokenEnv: "GATE_TOKEN_AGENT1"}}
	srv := realHookGate(t, cfg, nil)
	if ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_5", `{"command":"ls"}`), "--url", srv.URL, "--token-env", "HOOK_TOKEN"); ok || !strings.Contains(reason, "--agent") {
		t.Fatalf("caller token under the default agent name: allow=%v %q", ok, reason)
	}
	if ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_6", `{"command":"ls"}`), "--url", srv.URL, "--token-env", "HOOK_TOKEN", "--agent", "agent-1"); !ok {
		t.Fatalf("caller token under its own name: %s", reason)
	}
}

func TestGateHook_GateDownBlocks(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	srv := httptest.NewServer(http.NotFoundHandler())
	down := srv.URL
	srv.Close()
	ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_7", `{"command":"ls"}`), "--url", down)
	if ok || !strings.Contains(reason, "unreachable") {
		t.Fatalf("gate down: allow=%v %q", ok, reason)
	}
}

func TestGateHook_MalformedStdinBlocksInEveryFormat(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	for _, f := range []string{"claude", "codex", "copilot", "gemini"} {
		var out, errb bytes.Buffer
		code := gateHookCmd([]string{"--format", f, "--url", "http://127.0.0.1:1"}, strings.NewReader("{not json"), &out, &errb)
		if code != 0 || !strings.Contains(out.String(), "deny") || !strings.Contains(errb.String(), "could not be read") {
			t.Errorf("%s: exit %d out %q err %q", f, code, out.String(), errb.String())
		}
	}
}

func TestGateHook_UsageErrorExitsTwo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := gateHookCmd([]string{"--format", "cursor"}, strings.NewReader("{}"), &out, &errb); code != 2 || out.Len() != 0 {
		t.Fatalf("unknown format: exit %d out %q", code, out.String())
	}
	if code := gateHookCmd([]string{"--format", "claude", "--wait", "0s"}, strings.NewReader("{}"), &out, &errb); code != 2 {
		t.Fatalf("zero wait: exit %d", code)
	}
}

// fakeGate answers the hook's requests in a scripted way and records them.
type fakeGate struct {
	t        *testing.T
	mu       sync.Mutex
	ask      func() (int, ToolCallResponse)
	poll     func(n int) (int, ToolCallResponse)
	consume  func() (int, ToolCallResponse)
	polls    int
	revoked  string
	askBody  map[string]interface{}
	unsigned int
}

func (f *fakeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Authorization") != "Bearer "+hookTestToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		if _, err := authenticateWebhookSignature(r.Header.Get(webhookSigHeader), body, []byte(hookTestToken), 300, time.Now()); err != nil {
			f.unsigned++
		}
	}
	var code int
	var resp ToolCallResponse
	switch {
	case r.Method == http.MethodPost && r.URL.Path == toolGatePath:
		_ = json.Unmarshal(body, &f.askBody)
		code, resp = f.ask()
	case r.Method == http.MethodGet:
		f.polls++
		code, resp = f.poll(f.polls)
	case strings.HasSuffix(r.URL.Path, "/consume"):
		var b map[string]string
		_ = json.Unmarshal(body, &b)
		if b["binding_hash"] != "sha256:b1" {
			f.t.Errorf("consume binding %q", b["binding_hash"])
		}
		code, resp = f.consume()
	case strings.HasSuffix(r.URL.Path, "/revoke"):
		var b map[string]string
		_ = json.Unmarshal(body, &b)
		f.revoked = b["binding_hash"]
		code, resp = http.StatusOK, ToolCallResponse{Decision: "deny", State: "revoked"}
	default:
		code = http.StatusNotFound
	}
	writeToolDecision(w, code, resp)
}

func pendingAnswer() ToolCallResponse {
	return ToolCallResponse{ActionID: "a1", Decision: "pending", State: "pending", BindingHash: "sha256:b1"}
}

func allowedAnswer() ToolCallResponse {
	return ToolCallResponse{ActionID: "a1", Decision: "allow", State: "allowed", BindingHash: "sha256:b1", DecidedBy: "operator"}
}

func serveFake(t *testing.T, f *fakeGate) string {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestGateHook_PendingThenAllowedThroughPolling(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	f := &fakeGate{
		ask: func() (int, ToolCallResponse) { return http.StatusAccepted, pendingAnswer() },
		poll: func(n int) (int, ToolCallResponse) {
			if n < 2 {
				return http.StatusOK, pendingAnswer()
			}
			return http.StatusOK, allowedAnswer()
		},
		consume: func() (int, ToolCallResponse) {
			r := allowedAnswer()
			r.State, r.Permit = "consumed", "execute"
			return http.StatusOK, r
		},
	}
	url := serveFake(t, f)
	ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_p", `{"command":"ls"}`), "--url", url, "--agent", "agent-1")
	if !ok {
		t.Fatalf("approved after polling: %s", reason)
	}
	if f.polls != 2 || f.unsigned != 0 {
		t.Fatalf("polls %d, unsigned POSTs %d", f.polls, f.unsigned)
	}
	if f.askBody["agent"] != "agent-1" || f.askBody["run_id"] != "sess-1" || f.askBody["tool"] != "Bash" ||
		!strings.HasPrefix(f.askBody["action_id"].(string), "hook-") || f.askBody["wait"] == "" {
		t.Fatalf("ask body: %v", f.askBody)
	}
}

func TestGateHook_RefusedConsumeBlocks(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	for name, consume := range map[string]func() (int, ToolCallResponse){
		"409": func() (int, ToolCallResponse) {
			r := allowedAnswer()
			r.State, r.Decision, r.Reason = "consumed", "allow", "permit already consumed"
			return http.StatusConflict, r
		},
		"200 without permit": func() (int, ToolCallResponse) { return http.StatusOK, allowedAnswer() },
		"frozen": func() (int, ToolCallResponse) {
			r := allowedAnswer()
			r.Decision, r.Rule, r.Reason = "deny", "freeze", "tool gate frozen by operator"
			return http.StatusConflict, r
		},
	} {
		f := &fakeGate{
			ask:     func() (int, ToolCallResponse) { return http.StatusOK, allowedAnswer() },
			consume: consume,
		}
		url := serveFake(t, f)
		if ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_c", `{"command":"ls"}`), "--url", url); ok || !strings.Contains(reason, "not released") {
			t.Errorf("%s: allow=%v %q", name, ok, reason)
		}
	}
}

func TestGateHook_NoDecisionInTimeWithdrawsTheAsk(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	f := &fakeGate{
		ask:  func() (int, ToolCallResponse) { return http.StatusAccepted, pendingAnswer() },
		poll: func(int) (int, ToolCallResponse) { return http.StatusOK, pendingAnswer() },
	}
	url := serveFake(t, f)
	start := time.Now()
	ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_t", `{"command":"ls"}`), "--url", url, "--wait", "600ms")
	if ok || !strings.Contains(reason, "no decision within 600ms") {
		t.Fatalf("timeout: allow=%v %q", ok, reason)
	}
	if f.revoked != "sha256:b1" {
		t.Fatalf("the open ask must be revoked, got %q", f.revoked)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the hook overran its wait: %s", took)
	}
	if f.polls > 4 {
		t.Fatalf("a gate answering at once was polled %d times in 600ms", f.polls)
	}
}

func TestGateHook_GateWithoutDecisionBlocks(t *testing.T) {
	t.Setenv(hookDefaultTokenEnv, hookTestToken)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_x", `{"command":"ls"}`), "--url", srv.URL); ok || !strings.Contains(reason, "500") {
		t.Fatalf("500: allow=%v %q", ok, reason)
	}
	if ok, reason := runHookCmd(t, claudePayload("Bash", "toolu_y", `{"command":"ls"}`), "--url", "ftp://example.com"); ok || !strings.Contains(reason, "not an http(s) URL") {
		t.Fatalf("bad url: allow=%v %q", ok, reason)
	}
}
