package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// callersFixture serves the gate behind the same authenticator the engine
// mounts, with one shared token and two named callers.
func callersFixture(t *testing.T, requireCaller bool) http.Handler {
	t.Helper()
	t.Setenv("GATE_TOKEN_BILLING", "billing-token")
	t.Setenv("GATE_TOKEN_MAIL", "mail-token")
	cfg := gateCfg(config.ToolRule{Name: "create_invoice"}, config.ToolRule{Name: "send_email"})
	cfg.ToolGate.RequireCaller = requireCaller
	cfg.ToolGate.Callers = []config.GateCaller{
		{Name: "billing-agent", TokenEnv: "GATE_TOKEN_BILLING", Tools: []string{"create_invoice"}},
		{Name: "mail-agent", TokenEnv: "GATE_TOKEN_MAIL"},
	}
	gate := newToolGate(cfg, nil)
	wrap := func(next http.HandlerFunc) http.Handler {
		return newGateAuthenticator([]byte("shared-token"), cfg.ToolGate).middleware(1<<20, 300, false)(next)
	}
	mux := http.NewServeMux()
	mux.Handle(toolGatePath, wrap(gate.HandleCall))
	mux.Handle(toolGatePath+"/", wrap(gate.HandleStatus))
	return mux
}

func gateRequest(t *testing.T, h http.Handler, method, path, token, body string) (int, ToolCallResponse) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp ToolCallResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestGateCallers_TokenFixesTheAgentAndLimitsTools(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)

	code, r := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"inv-1","tool":"create_invoice","args":{"amount":10}}`)
	if code != http.StatusOK || r.Decision != "allow" {
		t.Fatalf("billing caller on its own tool: %d %+v", code, r)
	}
	a, err := state.ToolAction("inv-1")
	if err != nil || a.Agent != "billing-agent" {
		t.Fatalf("the action is recorded under the caller's name: %+v %v", a, err)
	}

	_, r = gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"mail-1","tool":"send_email","args":{}}`)
	if r.Decision != "deny" || r.Rule != "caller.tools" || r.DecidedBy != "allowlist" {
		t.Fatalf("a tool outside the caller's list is refused: %+v", r)
	}

	if code, _ := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"tool":"create_invoice","agent":"mail-agent","args":{}}`); code != http.StatusForbidden {
		t.Fatalf("a caller cannot claim another agent name: %d", code)
	}

	// A caller without a tools list may call every listed tool.
	if _, r := gateRequest(t, h, http.MethodPost, toolGatePath, "mail-token", `{"action_id":"mail-2","tool":"send_email","args":{}}`); r.Decision != "allow" {
		t.Fatalf("mail caller: %+v", r)
	}
}

func TestGateCallers_SeeOnlyTheirOwnActions(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)
	_, r := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"inv-7","tool":"create_invoice","args":{}}`)
	if r.Decision != "allow" {
		t.Fatalf("setup: %+v", r)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "mail-token", ""); code != http.StatusNotFound {
		t.Fatalf("another caller's action must look unknown, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodPost, toolGatePath+"/inv-7/consume", "mail-token", `{"binding_hash":"`+r.BindingHash+`"}`); code != http.StatusNotFound {
		t.Fatalf("another caller cannot consume the permit, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "billing-token", ""); code != http.StatusOK {
		t.Fatalf("the owner reads its action, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "shared-token", ""); code != http.StatusOK {
		t.Fatalf("the shared token keeps reaching every action, got %d", code)
	}
}

func TestGateCallers_SharedTokenAndRequireCaller(t *testing.T) {
	useReliabilityStore(t)
	open := callersFixture(t, false)
	if code, r := gateRequest(t, open, http.MethodPost, toolGatePath, "shared-token", `{"tool":"send_email","agent":"anything","args":{}}`); code != http.StatusOK || r.Decision != "allow" {
		t.Fatalf("without require_caller the shared token works as before: %d %+v", code, r)
	}
	if code, _ := gateRequest(t, open, http.MethodPost, toolGatePath, "nope", `{"tool":"send_email"}`); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token is refused, got %d", code)
	}
	strict := callersFixture(t, true)
	if code, _ := gateRequest(t, strict, http.MethodPost, toolGatePath, "shared-token", `{"tool":"send_email"}`); code != http.StatusUnauthorized {
		t.Fatalf("require_caller refuses the shared token, got %d", code)
	}
	if code, _ := gateRequest(t, strict, http.MethodPost, toolGatePath, "mail-token", `{"tool":"send_email","args":{}}`); code != http.StatusOK {
		t.Fatalf("require_caller still accepts a caller token, got %d", code)
	}
}

// A caller whose variable is empty cannot authenticate with an empty token.
func TestGateCallers_EmptyTokenNeverMatches(t *testing.T) {
	t.Setenv("GATE_TOKEN_EMPTY", "")
	auth := newGateAuthenticator([]byte("shared"), config.ToolGateConfig{
		Callers: []config.GateCaller{{Name: "x", TokenEnv: "GATE_TOKEN_EMPTY"}}})
	if _, _, ok := auth.identify("Bearer "); ok {
		t.Fatal("an empty bearer must never match")
	}
}

// A signed request is checked with the caller's own token, not the shared one.
func TestGateCallers_SignatureUsesTheCallerKey(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)
	body := []byte(`{"action_id":"sig-1","tool":"send_email","args":{}}`)
	send := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, toolGatePath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer mail-token")
		req.Header.Set(webhookSigHeader, signBody([]byte(key), time.Now().Unix(), body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("shared-token"); code != http.StatusUnauthorized {
		t.Fatalf("a body signed with the shared token under a caller token is refused, got %d", code)
	}
	if code := send("mail-token"); code != http.StatusOK {
		t.Fatalf("a body signed with the caller's token is accepted, got %d", code)
	}
}

// A caller's permit is spent only while the caller may still call the tool: a
// restart that takes a tool off its list ends what it was approved for before,
// and the operator hears of the refusal.
func TestGateCallers_ConsumeNeedsTheToolStillListed(t *testing.T) {
	useReliabilityStore(t)
	t.Setenv("GATE_TOKEN_BILLING", "billing-token")
	serve := func(tools ...string) (http.Handler, *countingChannel) {
		cfg := gateCfg(config.ToolRule{Name: "create_invoice", RequireApproval: true}, config.ToolRule{Name: "send_email"})
		cfg.ToolGate.Callers = []config.GateCaller{{Name: "billing-agent", TokenEnv: "GATE_TOKEN_BILLING", Tools: tools}}
		ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
		gate := newToolGate(cfg, ch)
		auth := newGateAuthenticator([]byte("shared-token"), cfg.ToolGate).middleware(1<<20, 300, false)
		mux := http.NewServeMux()
		mux.Handle(toolGatePath, auth(http.HandlerFunc(gate.HandleCall)))
		mux.Handle(toolGatePath+"/", auth(http.HandlerFunc(gate.HandleStatus)))
		return mux, ch
	}
	before, _ := serve("create_invoice", "send_email")
	_, r := gateRequest(t, before, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"inv-9","tool":"create_invoice","args":{"amount":900}}`)
	if r.State != "allowed" {
		t.Fatalf("setup: the operator approves: %+v", r)
	}

	after, ch := serve("send_email")
	consume := `{"binding_hash":"` + r.BindingHash + `"}`
	code, c := gateRequest(t, after, http.MethodPost, toolGatePath+"/inv-9/consume", "billing-token", consume)
	if code != http.StatusConflict || c.Permit != "" || c.Consume != "" || c.Decision != "deny" || c.Rule != "caller.tools" {
		t.Fatalf("consume once the tool left the caller's list = %d %+v", code, c)
	}
	if ch.noticeCount() != 1 || !strings.Contains(ch.notices[0], "caller billing-agent may not call create_invoice") {
		t.Fatalf("the operator is told: %q", ch.notices)
	}
	// the refused consume leaves its receipt, as every refused consume does
	refused := false
	if rows, err := state.ApprovalsForPipeline("tool-gate", 100); err == nil {
		for _, row := range rows {
			if row.ActionID == "inv-9" && row.Decision == "deny" && strings.HasPrefix(row.Policy, "permit not consumed") {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatalf("the refused consume left no receipt")
	}
	if a, err := state.ToolAction("inv-9"); err != nil || a.Status != "allowed" {
		t.Fatalf("the refused permit is not spent: %+v %v", a, err)
	}
	// The caller's other tools are unaffected.
	if _, r := gateRequest(t, after, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"mail-9","tool":"send_email","args":{}}`); r.Decision != "allow" {
		t.Fatalf("a listed tool is still allowed: %+v", r)
	}
}

// A permit is spent by whoever may still call its tool: a caller that lists
// the tool, a caller that lists no tools, and the shared token.
func TestGateCallers_ConsumeWhileTheToolIsAllowed(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)
	for _, c := range []struct{ token, id, tool string }{
		{"billing-token", "inv-3", "create_invoice"},
		{"mail-token", "mail-3", "send_email"},
		{"shared-token", "any-3", "create_invoice"},
	} {
		_, r := gateRequest(t, h, http.MethodPost, toolGatePath, c.token, `{"action_id":"`+c.id+`","tool":"`+c.tool+`","args":{}}`)
		if r.State != "allowed" {
			t.Fatalf("setup %s: %+v", c.id, r)
		}
		code, got := gateRequest(t, h, http.MethodPost, toolGatePath+"/"+c.id+"/consume", c.token, `{"binding_hash":"`+r.BindingHash+`"}`)
		if code != http.StatusOK || got.Permit != "execute" || got.Rule == "caller.tools" {
			t.Fatalf("%s consumes its permit for %s = %d %+v", c.token, c.tool, code, got)
		}
	}
}
